package testenv

import (
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
)

// TestDefaultAliasesMatchTheShippedPolicy is the guard that would have caught
// the drift this mechanism was built for.
//
// It asserts on the compiled-in defaults and the checked-in fixture, not on
// the Alias/AliasOther variables, so an override in the developer's shell
// cannot mask a broken default. And it needs neither the container nor the
// integration gate: the failure it looks for is entirely between two files in
// this repo, so it should be reported by `go test ./...` on a machine that has
// never seen the database.
//
// The history: testdata/local.yaml declared hrm_0424 for months after that
// snapshot had been dropped, and nothing failed. The one test using it asserts
// that a write is refused on policy grounds, and that refusal happens before
// anything connects — so the target's existence never entered into it.
func TestDefaultAliasesMatchTheShippedPolicy(t *testing.T) {
	pol, err := policy.Load(builtinPolicyPath())
	if err != nil {
		t.Fatalf("load %s: %v", builtinPolicyPath(), err)
	}

	byAlias := map[string]policy.Target{}
	declared := []string{}
	for _, tg := range pol.Targets {
		byAlias[tg.Alias] = tg
		declared = append(declared, tg.Alias)
	}

	for _, want := range []struct {
		alias    string
		writable bool
		role     string
	}{
		{defaultAlias, true, "defaultAlias (most tests; write tests need it writable)"},
		{defaultAliasOther, false, "defaultAliasOther (must be a target writes are refused against)"},
	} {
		tg, ok := byAlias[want.alias]
		if !ok {
			t.Errorf("%s = %q is not declared in testdata/local.yaml (declared: %v)",
				want.role, want.alias, declared)
			continue
		}
		if tg.Writable != want.writable {
			t.Errorf("%s = %q has writable=%v, want %v",
				want.role, want.alias, tg.Writable, want.writable)
		}
	}
}

// TestOverrideFallsBackAndRecords covers the two properties the announcement
// depends on: a blank or whitespace-only variable is not an override, and a
// real one is remembered so it can be reported.
//
// Whitespace matters more than it looks. `FOO= go test` and a trailing space
// pasted from a wiki both produce a set-but-empty variable, and treating that
// as "run against a target named empty string" would fail somewhere far from
// the cause.
func TestOverrideFallsBackAndRecords(t *testing.T) {
	saved := active
	t.Cleanup(func() { active = saved })

	for _, tc := range []struct {
		name    string
		set     bool
		val     string
		want    string
		records bool
	}{
		{name: "unset", set: false, want: "the-default"},
		{name: "empty", set: true, val: "", want: "the-default"},
		{name: "whitespace", set: true, val: "   ", want: "the-default"},
		{name: "value", set: true, val: "hrm_9999", want: "hrm_9999", records: true},
		{name: "trimmed", set: true, val: "  hrm_9999 ", want: "hrm_9999", records: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "HRM_SQL_MCP_TEST_UNIT_PROBE"
			if tc.set {
				t.Setenv(key, tc.val)
			}
			active = nil

			if got := override(key, "the-default"); got != tc.want {
				t.Errorf("override = %q, want %q", got, tc.want)
			}
			if recorded := len(active) > 0; recorded != tc.records {
				t.Errorf("recorded for announcement = %v, want %v (active=%v)",
					recorded, tc.records, active)
			}
		})
	}
}
