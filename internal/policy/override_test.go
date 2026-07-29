package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/envcfg"
)

// writePolicy puts a minimal valid policy on disk and returns its path.
func writePolicy(t *testing.T, extra string) string {
	t.Helper()
	body := `
server: {name: test, version: "0"}
profile: local
targets:
  - alias: hrm_0209
    host: 127.0.0.1
    database: hrm
    allow_cidrs: ["127.0.0.0/8"]
    credential_key: local_hrm
limits: {max_rows: 10}
paths: {sp_dir: "Stored Procedure", java_src_dir: src}
` + extra
	p := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// source builds an envcfg.Set backed only by process environment variables the
// test sets, so nothing on the developer's machine can influence the result.
func source(t *testing.T, kv map[string]string) *envcfg.Set {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
	s, err := envcfg.Load(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOverrideRewritesTarget(t *testing.T) {
	path := writePolicy(t, "")
	src := source(t, map[string]string{
		"HRM_SQL_TARGET_HRM_0209_DATABASE": "hrm_0511",
		"HRM_SQL_TARGET_HRM_0209_PORT":     "14330",
		"HRM_SQL_TARGET_HRM_0209_WRITABLE": "true",
	})

	p, applied, err := LoadWithOverrides(path, src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tgt, _ := p.TargetByAlias("hrm_0209")
	if tgt.Database != "hrm_0511" {
		t.Errorf("database = %q, want hrm_0511", tgt.Database)
	}
	if tgt.Port != 14330 {
		t.Errorf("port = %d, want 14330", tgt.Port)
	}
	if !tgt.Writable {
		t.Error("writable = false, want true")
	}
	// The file's own values must survive where nothing overrode them.
	if tgt.Host != "127.0.0.1" {
		t.Errorf("host = %q, want the policy's 127.0.0.1", tgt.Host)
	}
	if len(applied) != 3 {
		t.Errorf("recorded %d overrides, want 3: %v", len(applied), applied)
	}
}

// TestOverrideIsRecordedForDisplay is what keeps a layered configuration
// debuggable: a change nobody can see is indistinguishable from the file lying.
func TestOverrideIsRecordedForDisplay(t *testing.T) {
	path := writePolicy(t, "")
	src := source(t, map[string]string{"HRM_SQL_TARGET_HRM_0209_HOST": "127.0.0.2"})

	_, applied, err := LoadWithOverrides(path, src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("recorded %d overrides, want 1", len(applied))
	}
	got := applied[0].String()
	for _, want := range []string{"hrm_0209", "host", "127.0.0.2", "env", "HRM_SQL_TARGET_HRM_0209_HOST"} {
		if !strings.Contains(got, want) {
			t.Errorf("override display %q lacks %q", got, want)
		}
	}
}

// TestOverrideValidatesAfterApplying is the ordering the whole design rests
// on. An override that empties a required field must be refused, not shipped.
func TestOverrideValidatesAfterApplying(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			"blanked allowlist is a refusal, not permission",
			map[string]string{"HRM_SQL_TARGET_HRM_0209_ALLOW_CIDRS": " , "},
			"denies everything",
		},
		{
			"port must be a port",
			map[string]string{"HRM_SQL_TARGET_HRM_0209_PORT": "99999"},
			"not a valid port",
		},
		{
			"booleans must be booleans",
			map[string]string{"HRM_SQL_TARGET_HRM_0209_WRITABLE": "maybe"},
			"not a boolean",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writePolicy(t, "")
			_, _, err := LoadWithOverrides(path, source(t, tc.env))
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestExtraTargetDeclaredFromEnvironment(t *testing.T) {
	path := writePolicy(t, "")
	src := source(t, map[string]string{
		ExtraTargetsKey:                      "hrm_new",
		"HRM_SQL_TARGET_HRM_NEW_HOST":        "127.0.0.1",
		"HRM_SQL_TARGET_HRM_NEW_DATABASE":    "hrm_2026",
		"HRM_SQL_TARGET_HRM_NEW_ALLOW_CIDRS": "127.0.0.0/8",
	})

	p, _, err := LoadWithOverrides(path, src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tgt, ok := p.TargetByAlias("hrm_new")
	if !ok {
		t.Fatal("hrm_new was not added")
	}
	if tgt.Database != "hrm_2026" {
		t.Errorf("database = %q, want hrm_2026", tgt.Database)
	}
	// credential_key defaults to the alias, as for a file-declared target.
	if tgt.CredentialKey != "hrm_new" {
		t.Errorf("credential_key = %q, want hrm_new", tgt.CredentialKey)
	}
	if !strings.Contains(tgt.Note, ExtraTargetsKey) {
		t.Errorf("note = %q, should say it came from the environment", tgt.Note)
	}
}

// TestExtraTargetMustStateItsOwnNetworks: inheriting an allowlist would be a
// target nobody chose the blast radius for.
func TestExtraTargetMustStateItsOwnNetworks(t *testing.T) {
	path := writePolicy(t, "")
	src := source(t, map[string]string{
		ExtraTargetsKey:                   "hrm_new",
		"HRM_SQL_TARGET_HRM_NEW_HOST":     "127.0.0.1",
		"HRM_SQL_TARGET_HRM_NEW_DATABASE": "hrm_2026",
	})

	_, _, err := LoadWithOverrides(path, src)
	if err == nil {
		t.Fatal("expected a refusal for a target with no allow_cidrs")
	}
	if !strings.Contains(err.Error(), "does not inherit") {
		t.Errorf("error %q should explain that networks are not inherited", err)
	}
}

func TestExtraTargetCannotShadowTheFile(t *testing.T) {
	path := writePolicy(t, "")
	src := source(t, map[string]string{
		ExtraTargetsKey:                       "hrm_0209",
		"HRM_SQL_TARGET_HRM_0209_HOST":        "127.0.0.1",
		"HRM_SQL_TARGET_HRM_0209_DATABASE":    "elsewhere",
		"HRM_SQL_TARGET_HRM_0209_ALLOW_CIDRS": "127.0.0.0/8",
	})

	_, _, err := LoadWithOverrides(path, src)
	if err == nil {
		t.Fatal("expected a refusal for redeclaring a file target")
	}
	if !strings.Contains(err.Error(), "already defines") {
		t.Errorf("error %q should say the policy file already defines it", err)
	}
}

// TestNoSourceLeavesPolicyAlone pins that the feature is inert when unused.
func TestNoSourceLeavesPolicyAlone(t *testing.T) {
	path := writePolicy(t, "")
	p, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tgt, _ := p.TargetByAlias("hrm_0209")
	if tgt.Database != "hrm" || tgt.Host != "127.0.0.1" || tgt.Port != 1433 {
		t.Errorf("policy changed without any override: %+v", tgt)
	}
}
