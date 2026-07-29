package target

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/envcfg"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
)

// TestOverridesCannotReachProduction is the load-bearing test for making the
// connection settings environment-overridable at all.
//
// Host and allow_cidrs became configurable on request. The guarantee that had
// to survive that is the one the whole tool is built around: production is
// unreachable, and no amount of configuration makes it reachable. Here the
// environment is used as hostilely as it can be — pointing a target straight
// at the production address and opening its allowlist to the whole internet —
// and every case must still be refused by the compile-time denylists.
//
// If this test ever fails, the override feature has eaten the guard and must
// be reverted, not patched.
func TestOverridesCannotReachProduction(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		stage string
	}{
		{
			name: "point the host straight at production",
			env: map[string]string{
				"HRM_SQL_TARGET_LOCAL_HOST": "172.16.3.34",
			},
			stage: "host",
		},
		{
			name: "production address plus an allowlist that permits everything",
			env: map[string]string{
				"HRM_SQL_TARGET_LOCAL_HOST":        "172.16.3.34",
				"HRM_SQL_TARGET_LOCAL_ALLOW_CIDRS": "0.0.0.0/0",
			},
			stage: "host",
		},
		{
			name: "an innocuous name that resolves to production",
			env: map[string]string{
				"HRM_SQL_TARGET_LOCAL_HOST":        "prod.example.test",
				"HRM_SQL_TARGET_LOCAL_ALLOW_CIDRS": "0.0.0.0/0",
			},
			stage: "cidr",
		},
		{
			name: "several addresses, only one of them production",
			env: map[string]string{
				"HRM_SQL_TARGET_LOCAL_HOST":        "split.example.test",
				"HRM_SQL_TARGET_LOCAL_ALLOW_CIDRS": "0.0.0.0/0",
			},
			stage: "cidr",
		},
		{
			name: "decorated spelling of the production address",
			env: map[string]string{
				"HRM_SQL_TARGET_LOCAL_HOST":        "tcp:172.16.3.34,1433",
				"HRM_SQL_TARGET_LOCAL_ALLOW_CIDRS": "0.0.0.0/0",
			},
			stage: "host",
		},
		{
			name: "a target conjured entirely from the environment",
			env: map[string]string{
				policy.ExtraTargetsKey:              "sneaky",
				"HRM_SQL_TARGET_SNEAKY_HOST":        "172.16.3.34",
				"HRM_SQL_TARGET_SNEAKY_DATABASE":    "payroll",
				"HRM_SQL_TARGET_SNEAKY_ALLOW_CIDRS": "0.0.0.0/0",
			},
			stage: "host",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alias := "local"
			if _, ok := tc.env[policy.ExtraTargetsKey]; ok {
				alias = "sneaky"
			}
			pol := loadOverridden(t, tc.env)

			reg, err := NewRegistry(pol, pol.Profile, Credentials{
				Lookup: func(string, AccessMode) (string, string, bool) {
					return "u", "p", true
				},
			})
			if err != nil {
				t.Fatalf("registry: %v", err)
			}
			reg.WithResolver(resolver())

			_, err = reg.Check(context.Background(), alias, ReadOnly)
			if err == nil {
				t.Fatal("the guard allowed a target the environment pointed at production")
			}
			var ge *GuardError
			if !errors.As(err, &ge) {
				t.Fatalf("expected a GuardError, got %T: %v", err, err)
			}
			if ge.Stage != tc.stage {
				t.Errorf("rejected at stage %q, want %q (%s)", ge.Stage, tc.stage, ge.Detail)
			}
		})
	}
}

// TestOverrideStillAllowsLegitimateChange guards the other direction: a test
// that only proved things get refused would also pass if overrides did nothing.
func TestOverrideStillAllowsLegitimateChange(t *testing.T) {
	pol := loadOverridden(t, map[string]string{
		"HRM_SQL_TARGET_LOCAL_DATABASE": "hrm_0511",
		"HRM_SQL_TARGET_LOCAL_HOST":     "localhost",
	})
	reg, err := NewRegistry(pol, pol.Profile, Credentials{
		Lookup: func(string, AccessMode) (string, string, bool) { return "u", "p", true },
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	reg.WithResolver(resolver())

	tgt, err := reg.Check(context.Background(), "local", ReadOnly)
	if err != nil {
		t.Fatalf("guard refused a legitimate override: %v", err)
	}
	if tgt.Database() != "hrm_0511" {
		t.Errorf("database = %q, want hrm_0511", tgt.Database())
	}
	if tgt.Host() != "localhost" {
		t.Errorf("host = %q, want localhost", tgt.Host())
	}
}

// loadOverridden writes a minimal local policy, applies the given environment,
// and returns the resolved policy.
func loadOverridden(t *testing.T, env map[string]string) *policy.Policy {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	body := `
server: {name: test, version: "0"}
profile: local
targets:
  - alias: local
    host: 127.0.0.1
    database: hrm
    allow_cidrs: ["127.0.0.0/8"]
paths: {sp_dir: sp, java_src_dir: src}
`
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := envcfg.Load(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	pol, _, err := policy.LoadWithOverrides(path, src)
	if err != nil {
		t.Fatalf("load policy with overrides: %v", err)
	}
	return pol
}
