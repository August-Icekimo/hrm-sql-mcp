package target

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/codex-k8s/hrm-sql-mcp/internal/constants"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
)

// Resolver looks up host names. Injected so the guard can be tested without
// touching DNS; production uses net.DefaultResolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// GuardError describes why a target was rejected. It deliberately carries the
// stage so failures are diagnosable without turning on debug logging — the
// difference between "your DNS is broken" and "you tried to reach production"
// matters a great deal to whoever is reading the error.
type GuardError struct {
	Stage  string
	Alias  string
	Host   string
	Detail string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("target %q rejected at %s: %s (host=%q)", e.Alias, e.Stage, e.Detail, e.Host)
}

// normaliseHost strips the decorations SQL Server accepts around a host so that
// denylist matching cannot be defeated by spelling.
//
// SQL Server clients tolerate all of these for one address:
//
//	"172.16.3.34"          plain
//	" 172.16.3.34 "        padded
//	"tcp:172.16.3.34"      protocol prefix
//	"172.16.3.34,1433"     comma port
//	"172.16.3.34\SQLEXPR"  named instance
//	"TCP:172.16.3.34"      any case
//
// A denylist that only matches the plain form is decorative. Everything above
// must collapse to the same string before comparison.
func normaliseHost(h string) string {
	s := strings.TrimSpace(h)
	s = strings.ToLower(s)

	// Protocol prefixes: tcp:, np:, lpc:
	for _, p := range []string{"tcp:", "np:", "lpc:"} {
		if strings.HasPrefix(s, p) {
			s = s[len(p):]
			break
		}
	}
	// Named instance: host\INSTANCE
	if i := strings.IndexByte(s, '\\'); i >= 0 {
		s = s[:i]
	}
	// Comma port: host,1433
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	// Bracketed IPv6 literal: [::1]
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")

	return strings.TrimSpace(s)
}

// check runs the four gates in order. Any one of them rejecting is final.
//
// The order is deliberate: cheap and offline checks first, so an obviously bad
// target is refused without emitting a DNS query that would itself leak intent.
func check(ctx context.Context, res Resolver, profile string, t policy.Target) (string, []netip.Addr, error) {
	alias := t.Alias

	// ---- Gate 1: profile must be explicit and permitted -------------------
	if profile == "" {
		return "", nil, &GuardError{Stage: "profile", Alias: alias, Host: t.Host,
			Detail: "HRM_SQL_MCP_PROFILE is empty; it has no default and must be set explicitly"}
	}
	if slices.Contains(constants.DeniedProfileNames, strings.ToLower(profile)) {
		return "", nil, &GuardError{Stage: "profile", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("profile %q is permanently denied", profile)}
	}
	if !slices.Contains(constants.AllowedProfiles, profile) {
		return "", nil, &GuardError{Stage: "profile", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("profile %q is not one of %v", profile, constants.AllowedProfiles)}
	}

	// ---- Gate 2: host denylist, after normalisation ------------------------
	host := normaliseHost(t.Host)
	if host == "" {
		return "", nil, &GuardError{Stage: "host", Alias: alias, Host: t.Host,
			Detail: "host is empty after normalisation"}
	}
	if slices.Contains(constants.DeniedHosts, host) {
		return "", nil, &GuardError{Stage: "host", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("%q is on the permanent host denylist", host)}
	}

	// ---- Gate 3: resolve; failure to resolve is a rejection ----------------
	//
	// Fail closed. If we cannot determine where a name points, we do not know
	// whether it points at production, and "don't know" is not "allowed".
	addrs, err := res.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return "", nil, &GuardError{Stage: "resolve", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("cannot resolve: %v", err)}
	}
	if len(addrs) == 0 {
		return "", nil, &GuardError{Stage: "resolve", Alias: alias, Host: t.Host,
			Detail: "resolved to no addresses"}
	}

	// ---- Gate 4: every resolved address must pass both CIDR tests ----------
	//
	// "Every" is the load-bearing word. A name with several A records only
	// needs one of them inside the production range for the connection to
	// land there, so a single bad address rejects the whole target.
	denied, err := parseCIDRs(constants.DeniedCIDRs)
	if err != nil {
		return "", nil, &GuardError{Stage: "cidr", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("built-in denylist is malformed: %v", err)}
	}
	allowed, err := parseCIDRs(t.AllowCIDRs)
	if err != nil {
		return "", nil, &GuardError{Stage: "cidr", Alias: alias, Host: t.Host,
			Detail: fmt.Sprintf("allow_cidrs is malformed: %v", err)}
	}
	if len(allowed) == 0 {
		return "", nil, &GuardError{Stage: "cidr", Alias: alias, Host: t.Host,
			Detail: "allow_cidrs is empty, which denies everything"}
	}

	for _, a := range addrs {
		a = a.Unmap()
		for _, d := range denied {
			if d.Contains(a) {
				return "", nil, &GuardError{Stage: "cidr", Alias: alias, Host: t.Host,
					Detail: fmt.Sprintf("%s is inside denied network %s", a, d)}
			}
		}
		if !slices.ContainsFunc(allowed, func(p netip.Prefix) bool { return p.Contains(a) }) {
			return "", nil, &GuardError{Stage: "cidr", Alias: alias, Host: t.Host,
				Detail: fmt.Sprintf("%s is outside allow_cidrs %v", a, t.AllowCIDRs)}
		}
	}

	return host, addrs, nil
}

func parseCIDRs(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%q: %w", s, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// systemResolver adapts net.Resolver to the Resolver interface.
type systemResolver struct{ r *net.Resolver }

func (s systemResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return s.r.LookupNetIP(ctx, network, host)
}
