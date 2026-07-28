package target

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// AccessMode selects which credential set a connection is opened with.
type AccessMode int

const (
	// ReadOnly uses the login that has DENY EXECUTE and no writer role.
	// Every read-only tool must use this, including schema inspection.
	ReadOnly AccessMode = iota
	// ReadWrite uses the login permitted to write and to create procedures.
	// Only reachable through the approval-gated tools.
	ReadWrite
)

func (m AccessMode) String() string {
	if m == ReadWrite {
		return "readwrite"
	}
	return "readonly"
}

// Target is a guard-approved database endpoint.
//
// Every field is unexported and there is no exported constructor: the only way
// to hold one is to receive it from Registry.Open, which cannot be reached
// without passing the guard. See the package comment.
type Target struct {
	alias    string
	host     string
	port     int
	database string
	appName  string
	writable bool
	mode     AccessMode
	addrs    []netip.Addr
}

// Alias returns the policy alias. Safe to log.
func (t *Target) Alias() string { return t.alias }

// Host returns the normalised host. Safe to log.
func (t *Target) Host() string { return t.host }

// Database returns the initial catalog. Safe to log.
func (t *Target) Database() string { return t.database }

// Mode returns the access mode this target was opened for.
func (t *Target) Mode() AccessMode { return t.mode }

// Addr renders host:port for display.
func (t *Target) Addr() string { return net.JoinHostPort(t.host, strconv.Itoa(t.port)) }

// ResolvedAddrs returns the addresses the guard verified, for audit records.
func (t *Target) ResolvedAddrs() []string {
	out := make([]string, 0, len(t.addrs))
	for _, a := range t.addrs {
		out = append(out, a.String())
	}
	return out
}

// Describe returns the envelope header every tool response carries, so an
// agent never has to guess which database it is talking to.
func (t *Target) Describe(login string) map[string]string {
	return map[string]string{
		"alias":    t.alias,
		"server":   t.Addr(),
		"database": t.database,
		"login":    login,
		"mode":     t.mode.String(),
	}
}

// dsn builds the connection string. Unexported on purpose: the assembled DSN
// contains the password and must never leave this package.
func (t *Target) dsn(user, password string, encrypt, trustCert bool) string {
	q := fmt.Sprintf("sqlserver://%s:%s@%s:%d?database=%s&app+name=%s",
		urlEscape(user), urlEscape(password), t.host, t.port, urlEscape(t.database), urlEscape(t.appName))
	if encrypt {
		q += "&encrypt=true"
	} else {
		q += "&encrypt=disable"
	}
	if trustCert {
		q += "&trustservercertificate=true"
	}
	return q
}

// ErrNotWritable is returned when ReadWrite is requested for a target the
// policy did not mark writable.
var ErrNotWritable = fmt.Errorf("target is not writable in this policy")

// ensureMode checks the requested mode against the policy flag.
func (t *Target) ensureMode(m AccessMode) error {
	if m == ReadWrite && !t.writable {
		return fmt.Errorf("%w: %s", ErrNotWritable, t.alias)
	}
	return nil
}
