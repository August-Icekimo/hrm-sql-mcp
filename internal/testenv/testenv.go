// Package testenv opens the local development database for integration tests.
//
// It is test-only, and it fails rather than falls back. A helper that quietly
// substituted a different target when the expected one was unavailable would
// let an integration suite pass against something nobody meant to test — which
// for a tool whose central claim is "it only touches the database you named"
// would be self-defeating.
package testenv

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	yaml "github.com/yaml/go-yaml"

	"github.com/codex-k8s/hrm-sql-mcp/internal/config"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// EnvGate must be set for integration tests to run. They need the podman
// container from the README, so they are opt-in rather than skipped-by-guess.
const EnvGate = "HRM_SQL_MCP_IT"

// Alias is the local development target declared in testdata/local.yaml.
const Alias = "local_hrm"

// Open returns a read-only connection to the local development database,
// skipping the test when the gate is not set.
func Open(t *testing.T) (*sql.DB, *policy.Policy) {
	t.Helper()
	if os.Getenv(EnvGate) == "" {
		t.Skipf("set %s=1 to run integration tests against the local container", EnvGate)
	}

	pol, err := policy.Load(PolicyPath())
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	store, err := config.LoadCredentials(cfg.CredentialsPath)
	if err != nil {
		t.Fatalf("load credentials: %v", err)
	}
	reg, err := target.NewRegistry(pol, pol.Profile, store.Credentials())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })

	db, _, err := reg.Open(context.Background(), Alias, target.ReadOnly)
	if err != nil {
		t.Fatalf("open %s: %v", Alias, err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s (is the container running?): %v", Alias, err)
	}
	return db, pol
}

// PolicyPath locates testdata/local.yaml relative to this source file, so the
// tests do not depend on which directory `go test` was invoked from.
func PolicyPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "local.yaml")
}

// EnvProjectRoot points at a checkout of the served project (for HRM: the repo
// root). Tests that read its "Stored Procedure" or "src" directories skip
// without it.
const EnvProjectRoot = "HRM_PROJECT_ROOT"

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
	if os.Getenv(EnvGate) == "" {
		t.Skipf("set %s=1 to run integration tests against the local container", EnvGate)
	}

	pol, err := policy.Load(PolicyPath())
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
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
