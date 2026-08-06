// Package testenv opens the local development database for integration tests.
//
// It is test-only, and it fails rather than falls back. A helper that quietly
// substituted a different target when the expected one was unavailable would
// let an integration suite pass against something nobody meant to test — which
// for a tool whose central claim is "it only touches the database you named"
// would be self-defeating.
//
// # Pointing the suite somewhere else
//
// The snapshots on the development container get created and dropped over
// time, so the defaults below will drift out of date. Three environment
// variables move the suite without editing Go source:
//
//	HRM_SQL_MCP_TEST_POLICY       policy file (default: testdata/local.yaml)
//	HRM_SQL_MCP_TEST_ALIAS        primary, writable target (default: hrm_0209)
//	HRM_SQL_MCP_TEST_ALIAS_OTHER  read-only target (default: hrm_0805)
//
// What keeps this from becoming the silent-substitution problem the package
// exists to avoid is the checking, not the reporting:
//
//   - The aliases are checked against the policy before anything connects,
//     including that the writable one is writable and the other is not. A
//     stale alias fails with a message naming what is actually declared,
//     instead of surfacing later as an opaque connection error — or, worse,
//     as a test that still passes because its assertion never needed the
//     target to exist.
//   - Active overrides are printed once per run, which helps when reading a
//     failure but is not a guarantee: `go test` discards a passing package's
//     output, stderr included. See announce.
package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	yaml "github.com/yaml/go-yaml"

	"github.com/codex-k8s/hrm-sql-mcp/internal/config"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// EnvGate must be set for integration tests to run. They need the podman
// container from the README, so they are opt-in rather than skipped-by-guess.
const EnvGate = "HRM_SQL_MCP_IT"

// EnvProjectRoot points at a checkout of the served project (for HRM: the repo
// root). Tests that read its "Stored Procedure" or "src" directories skip
// without it.
const EnvProjectRoot = "HRM_PROJECT_ROOT"

// Overrides for what the suite runs against. Named HRM_SQL_MCP_TEST_* rather
// than reusing the engine's HRM_SQL_MCP_* names on purpose: these only steer
// the test harness, and a variable that looked like engine configuration would
// invite someone to export it in a shell that also runs the real binary.
const (
	EnvTestPolicy     = "HRM_SQL_MCP_TEST_POLICY"
	EnvTestAlias      = "HRM_SQL_MCP_TEST_ALIAS"
	EnvTestAliasOther = "HRM_SQL_MCP_TEST_ALIAS_OTHER"
)

// Defaults, kept in step with testdata/local.yaml.
//
// defaultAlias is the writable one because it maps to database `hrm`, the only
// snapshot where the read-write login has a USER. The others have hrm_mcp_ro
// only, so a write against them fails at login rather than on policy — which
// is a much less useful thing for a test to prove.
const (
	defaultAlias      = "hrm_0209"
	defaultAliasOther = "hrm_0805"
)

// Alias is the primary local development target: writable, used by most tests.
var Alias = override(EnvTestAlias, defaultAlias)

// AliasOther is a second snapshot on the same server that the policy marks
// read-only, for comparison tests and for proving writes are refused.
var AliasOther = override(EnvTestAliasOther, defaultAliasOther)

// policyFile is resolved once so that an override is recorded at init time
// alongside the others, rather than on whichever call happens to be first.
var policyFile = override(EnvTestPolicy, builtinPolicyPath())

// active records every override for the announcement. Appended to only from
// package-level var initialisation, which Go runs on one goroutine.
var active []string

func override(env, def string) string {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	active = append(active, fmt.Sprintf("  %s=%s  (default %s)", env, v, def))
	return v
}

var announceOnce sync.Once

// announce prints the active overrides to stderr exactly once per run.
//
// What this actually guarantees, measured rather than assumed: the message
// appears whenever a package fails, and under -v. It does NOT appear on a
// fully green run without -v — `go test` discards a passing package's output,
// and it makes no exception for stderr. Writing here instead of t.Logf buys
// ordering, not visibility.
//
// So this is a convenience for reading a failure, not a safety mechanism. The
// safety mechanism is checkAliases: it refuses to run against an alias the
// policy does not declare, which is the failure mode that actually matters.
// Do not lean on this announcement to notice a misdirected suite — by the
// time it would matter, the run is green and the message is gone.
func announce() {
	announceOnce.Do(func() {
		if len(active) == 0 {
			return
		}
		fmt.Fprintf(os.Stderr,
			"\n[testenv] integration targets overridden by environment:\n%s\n\n",
			strings.Join(active, "\n"))
	})
}

// builtinPolicyPath locates testdata/local.yaml relative to this source file,
// so the tests do not depend on which directory `go test` was invoked from.
func builtinPolicyPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "local.yaml")
}

// PolicyPath is the policy the suite runs against.
func PolicyPath() string { return policyFile }

// loadPolicy is the single entry point for everything here: it gates on the
// integration flag, announces overrides, loads the policy and checks that the
// aliases still mean what the tests assume.
func loadPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	if os.Getenv(EnvGate) == "" {
		t.Skipf("set %s=1 to run integration tests against the local container", EnvGate)
	}
	announce()

	pol, err := policy.Load(PolicyPath())
	if err != nil {
		t.Fatalf("load policy %s: %v", PolicyPath(), err)
	}
	checkAliases(t, pol)
	return pol
}

// checkAliases fails when the configured aliases no longer match the policy.
//
// Checked on every entry point rather than only where each alias is used. The
// case that motivated this: AliasOther pointed at a snapshot that had been
// dropped months earlier, and the one test using it still passed — it asserts
// that a write is refused for being read-only, and that refusal happens before
// anything connects. The fixture was broken and the suite was green. Verifying
// the fixture itself, everywhere, is the only version of this check that would
// have caught it.
func checkAliases(t *testing.T, pol *policy.Policy) {
	t.Helper()

	byAlias := make(map[string]policy.Target, len(pol.Targets))
	declared := make([]string, 0, len(pol.Targets))
	for _, tg := range pol.Targets {
		byAlias[tg.Alias] = tg
		declared = append(declared, tg.Alias)
	}

	need := []struct {
		alias    string
		env      string
		writable bool
		why      string
	}{
		{Alias, EnvTestAlias, true,
			"most tests use it, and the write tests need the read-write login to be allowed"},
		{AliasOther, EnvTestAliasOther, false,
			"its whole purpose is to be a target the policy refuses writes against"},
	}

	for _, n := range need {
		tg, ok := byAlias[n.alias]
		if !ok {
			t.Fatalf("alias %q is not declared in %s (declared: %s)\n"+
				"\tThe snapshot was probably dropped. Point the suite at a current one:\n"+
				"\t\t%s=<alias> go test ./...",
				n.alias, PolicyPath(), strings.Join(declared, ", "), n.env)
		}
		if tg.Writable != n.writable {
			t.Fatalf("alias %q has writable=%v in %s, tests need %v — %s\n"+
				"\tPick a different alias with %s=<alias>.",
				n.alias, tg.Writable, PolicyPath(), n.writable, n.why, n.env)
		}
	}
}

// open is the shared body of Open and OpenWritable.
func open(t *testing.T, mode target.AccessMode) (*sql.DB, *policy.Policy) {
	t.Helper()
	pol := loadPolicy(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	src, err := cfg.LoadSource()
	if err != nil {
		t.Fatalf("load credential source: %v", err)
	}
	store := config.NewStore(src)
	reg, err := target.NewRegistry(pol, pol.Profile, store.Credentials())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })

	db, _, err := reg.Open(context.Background(), Alias, mode)
	if err != nil {
		t.Fatalf("open %s (%v): %v", Alias, mode, err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s (is the container running?): %v", Alias, err)
	}
	return db, pol
}

// Open returns a read-only connection to the local development database,
// skipping the test when the gate is not set.
func Open(t *testing.T) (*sql.DB, *policy.Policy) {
	t.Helper()
	return open(t, target.ReadOnly)
}

// OpenWritable returns a read-write connection to the writable snapshot.
//
// Separate from Open so that a test which does not need write access cannot
// get it by accident: most of this suite runs read-only, and the handful of
// tests that mutate rows should be visible as such at the call site.
//
// checkAliases has already established that Alias is marked writable, so a
// failure here is about the login or the container, not the policy.
func OpenWritable(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := open(t, target.ReadWrite)
	return db
}

// RequireIntegration skips a test unless the gate is set, and configures the
// environment that service.New reads.
//
// The policy is copied to a temporary file with its audit path redirected into
// the test's own directory. Tests must not append to the developer's real
// audit log: that file is the record of what actually touched the database,
// and salting it with synthetic rows damages the one artefact this project
// treats as evidence. Redirecting rather than disabling keeps the property
// that auditing has no off switch.
func RequireIntegration(t *testing.T) {
	t.Helper()
	pol := loadPolicy(t)

	dir := t.TempDir()
	pol.Audit.File = filepath.Join(dir, "audit.jsonl")
	pol.Audit.SnapshotDir = filepath.Join(dir, "snapshots")

	raw, err := yaml.Marshal(pol)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	t.Setenv("HRM_SQL_MCP_PROFILE", pol.Profile)
	t.Setenv("HRM_SQL_MCP_POLICY", path)
	t.Setenv("HRM_SQL_MCP_ACTOR", "test")
	if root := os.Getenv(EnvProjectRoot); root != "" {
		t.Setenv("HRM_SQL_MCP_PROJECT_ROOT", root)
	}
}

// RequireProjectRoot skips a test that needs the served project's files.
func RequireProjectRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv(EnvProjectRoot)
	if root == "" {
		t.Skipf("set %s to a checkout of the served project to run this test", EnvProjectRoot)
	}
	return root
}
