package target

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/constants"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
)

// fakeResolver maps names to addresses without touching DNS, so this whole
// file runs offline and deterministically. A missing entry resolves to an
// error, which is itself one of the cases under test.
type fakeResolver struct{ m map[string][]string }

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	ips, ok := f.m[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, s := range ips {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func resolver() fakeResolver {
	return fakeResolver{m: map[string][]string{
		"172.16.3.34":          {"172.16.3.34"},  // production, denied by host and CIDR
		"172.22.1.130":         {"172.22.1.130"}, // UAT, allowed
		"127.0.0.1":            {"127.0.0.1"},    // local container
		"localhost":            {"127.0.0.1"},
		"prod.example.test":    {"172.16.3.34"},                 // innocuous name, production address
		"split.example.test":   {"172.22.1.130", "172.16.3.34"}, // one good, one bad
		"outside.example.test": {"203.0.113.9"},                 // resolves, but outside allow_cidrs
	}}
}

func uatTarget(host string) policy.Target {
	return policy.Target{
		Alias:      "uat_hrm",
		Host:       host,
		Port:       1433,
		Database:   "hrm",
		AllowCIDRs: []string{"172.22.1.0/24", "127.0.0.0/8"},
	}
}

// TestNormaliseHost pins the spellings a denylist must survive.
//
// Every entry here is a form SQL Server clients accept for the same address.
// If normalisation misses any one of them, the production denylist becomes
// decorative for that spelling.
func TestNormaliseHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"172.16.3.34", "172.16.3.34"},
		{"  172.16.3.34  ", "172.16.3.34"},
		{"172.16.3.34,1433", "172.16.3.34"},
		{"tcp:172.16.3.34", "172.16.3.34"},
		{"TCP:172.16.3.34", "172.16.3.34"},
		{"np:172.16.3.34", "172.16.3.34"},
		{`172.16.3.34\SQLEXPRESS`, "172.16.3.34"},
		{`tcp:172.16.3.34\SQLEXPRESS,1433`, "172.16.3.34"},
		{"  TCP:172.16.3.34 ,1433 ", "172.16.3.34"},
		{"[::1]", "::1"},
		{"MyHost.Example.TEST", "myhost.example.test"},
	}
	for _, c := range cases {
		if got := normaliseHost(c.in); got != c.want {
			t.Errorf("normaliseHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGuardRejectsProduction is the evidence behind the claim that this tool
// cannot reach production. Each case is a different way someone might get
// there — by spelling, by name, by a second A record, or by profile.
func TestGuardRejectsProduction(t *testing.T) {
	res := resolver()
	cases := []struct {
		name    string
		profile string
		target  policy.Target
		stage   string
	}{
		{"plain production IP", constants.ProfileUAT, uatTarget("172.16.3.34"), "host"},
		{"padded", constants.ProfileUAT, uatTarget("  172.16.3.34  "), "host"},
		{"comma port", constants.ProfileUAT, uatTarget("172.16.3.34,1433"), "host"},
		{"tcp prefix", constants.ProfileUAT, uatTarget("tcp:172.16.3.34"), "host"},
		{"uppercase prefix", constants.ProfileUAT, uatTarget("TCP:172.16.3.34"), "host"},
		{"named instance", constants.ProfileUAT, uatTarget(`172.16.3.34\SQLEXPRESS`), "host"},
		{"combined decorations", constants.ProfileUAT, uatTarget(`tcp:172.16.3.34\SQLEXPRESS,1433`), "host"},

		// The name is not on the denylist; only resolution reveals the address.
		{"hostname pointing at production", constants.ProfileUAT, uatTarget("prod.example.test"), "cidr"},
		// Multiple A records: one inside production is enough to reject.
		{"split horizon, one bad record", constants.ProfileUAT, uatTarget("split.example.test"), "cidr"},
		// Fail closed on unresolvable names.
		{"unresolvable host", constants.ProfileUAT, uatTarget("nowhere.example.test"), "resolve"},
		// Resolves fine, but outside the policy allowlist.
		{"outside allow_cidrs", constants.ProfileUAT, uatTarget("outside.example.test"), "cidr"},

		// Profile gates.
		{"empty profile", "", uatTarget("172.22.1.130"), "profile"},
		{"prod profile", "prod", uatTarget("172.22.1.130"), "profile"},
		{"production profile", "production", uatTarget("172.22.1.130"), "profile"},
		{"unknown profile", "staging", uatTarget("172.22.1.130"), "profile"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := check(context.Background(), res, c.profile, c.target)
			if err == nil {
				t.Fatalf("expected rejection, got none")
			}
			var ge *GuardError
			if !errors.As(err, &ge) {
				t.Fatalf("expected *GuardError, got %T: %v", err, err)
			}
			if ge.Stage != c.stage {
				t.Errorf("rejected at stage %q, expected %q (err: %v)", ge.Stage, c.stage, err)
			}
		})
	}
}

// TestGuardRejectsEmptyAllowCIDRs pins the fail-closed behaviour that matters
// most: an absent allowlist must deny everything, not permit everything.
// This is the single most common way an allowlist silently becomes a no-op.
func TestGuardRejectsEmptyAllowCIDRs(t *testing.T) {
	tgt := uatTarget("172.22.1.130")
	tgt.AllowCIDRs = nil
	_, _, err := check(context.Background(), resolver(), constants.ProfileUAT, tgt)
	if err == nil {
		t.Fatal("empty allow_cidrs must be treated as deny-all")
	}
	if !strings.Contains(err.Error(), "denies everything") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGuardAllowsPermitted confirms the guard is not simply refusing
// everything — a test suite that only asserts rejections would pass with a
// guard that never lets anything through.
func TestGuardAllowsPermitted(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		target  policy.Target
	}{
		{"UAT by IP", constants.ProfileUAT, uatTarget("172.22.1.130")},
		{"UAT with decorations", constants.ProfileUAT, uatTarget("tcp:172.22.1.130,1433")},
		{"local container", constants.ProfileLocal, func() policy.Target {
			t := uatTarget("127.0.0.1")
			t.Alias = "local_hrm"
			return t
		}()},
		{"localhost name", constants.ProfileLocal, func() policy.Target {
			t := uatTarget("localhost")
			t.Alias = "local_hrm"
			return t
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, addrs, err := check(context.Background(), resolver(), c.profile, c.target)
			if err != nil {
				t.Fatalf("expected pass, got %v", err)
			}
			if host == "" || len(addrs) == 0 {
				t.Fatalf("guard passed but returned host=%q addrs=%v", host, addrs)
			}
		})
	}
}

// TestDenylistsAreWellFormed guards the guard: a typo in a compile-time CIDR
// would make the denylist silently ineffective, and nothing else in the suite
// would notice.
func TestDenylistsAreWellFormed(t *testing.T) {
	if len(constants.DeniedCIDRs) == 0 || len(constants.DeniedHosts) == 0 {
		t.Fatal("denylists must not be empty")
	}
	if _, err := parseCIDRs(constants.DeniedCIDRs); err != nil {
		t.Fatalf("DeniedCIDRs malformed: %v", err)
	}
	for _, h := range constants.DeniedHosts {
		if got := normaliseHost(h); got != h {
			t.Errorf("DeniedHosts entry %q is not in normalised form (would never match; want %q)", h, got)
		}
	}
	for _, p := range constants.AllowedProfiles {
		for _, d := range constants.DeniedProfileNames {
			if p == d {
				t.Errorf("profile %q appears in both AllowedProfiles and DeniedProfileNames", p)
			}
		}
	}
}
