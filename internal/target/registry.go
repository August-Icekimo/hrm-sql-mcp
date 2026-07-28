package target

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"sync"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
)

// Credentials supplies the two logins. Values come from a 0600 file outside
// any repository; see package config.
type Credentials struct {
	// Lookup returns user and password for an alias and mode.
	// Returning ok=false must be treated as "no credential, do not connect".
	Lookup func(alias string, mode AccessMode) (user, password string, ok bool)
}

// Registry is the sole factory for connections.
type Registry struct {
	pol     *policy.Policy
	profile string
	creds   Credentials
	res     Resolver

	mu    sync.Mutex
	pools map[string]*sql.DB
}

// NewRegistry builds a registry bound to one policy and profile.
//
// The profile is passed in rather than read from the environment here so that
// the caller has to have obtained it explicitly, and so tests can drive it.
func NewRegistry(pol *policy.Policy, profile string, creds Credentials) (*Registry, error) {
	if pol == nil {
		return nil, fmt.Errorf("policy is required")
	}
	// The policy declares which environment it is for; the process states which
	// environment it believes it is running as. If those disagree, refuse.
	// This is what stops "I thought I was on local" from becoming a UAT write.
	if pol.Profile != profile {
		return nil, fmt.Errorf(
			"profile mismatch: policy declares %q but HRM_SQL_MCP_PROFILE is %q",
			pol.Profile, profile)
	}
	if creds.Lookup == nil {
		return nil, fmt.Errorf("credentials lookup is required")
	}
	return &Registry{
		pol:     pol,
		profile: profile,
		creds:   creds,
		res:     systemResolver{r: net.DefaultResolver},
		pools:   make(map[string]*sql.DB),
	}, nil
}

// WithResolver overrides DNS resolution. Test-only seam.
func (r *Registry) WithResolver(res Resolver) *Registry {
	r.res = res
	return r
}

// Aliases lists the declared target aliases.
func (r *Registry) Aliases() []string {
	out := make([]string, 0, len(r.pol.Targets))
	for _, t := range r.pol.Targets {
		out = append(out, t.Alias)
	}
	return out
}

// Check runs the guard for an alias without opening a connection.
// Used by mssql_targets so an agent can see what is reachable, and by tests.
func (r *Registry) Check(ctx context.Context, alias string, mode AccessMode) (*Target, error) {
	pt, ok := r.pol.TargetByAlias(alias)
	if !ok {
		return nil, fmt.Errorf("unknown target alias %q (declared: %v)", alias, r.Aliases())
	}
	host, addrs, err := check(ctx, r.res, r.profile, pt)
	if err != nil {
		return nil, err
	}
	t := &Target{
		alias:    pt.Alias,
		host:     host,
		port:     pt.Port,
		database: pt.Database,
		appName:  pt.AppName,
		writable: pt.Writable,
		mode:     mode,
		addrs:    addrs,
	}
	if err := t.ensureMode(mode); err != nil {
		return nil, err
	}
	return t, nil
}

// Open returns a pooled connection for an alias.
//
// This is the only function in the program that produces a *sql.DB, and it
// cannot run without check() having passed first.
func (r *Registry) Open(ctx context.Context, alias string, mode AccessMode) (*sql.DB, *Target, error) {
	t, err := r.Check(ctx, alias, mode)
	if err != nil {
		return nil, nil, err
	}

	user, password, ok := r.creds.Lookup(alias, mode)
	if !ok {
		return nil, nil, fmt.Errorf("no %s credential configured for target %q", mode, alias)
	}

	pt, _ := r.pol.TargetByAlias(alias)
	encrypt := pt.Encrypt == nil || *pt.Encrypt

	key := alias + "/" + mode.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if db, hit := r.pools[key]; hit {
		return db, t, nil
	}

	db, err := sql.Open("sqlserver", t.dsn(user, password, encrypt, pt.TrustServerCertificate))
	if err != nil {
		// Never wrap the DSN into the error: it contains the password.
		return nil, nil, fmt.Errorf("open %s (%s): %w", alias, mode, err)
	}
	// Small pools on purpose. This is a single-user developer tool, and each
	// MCP client spawns its own process — a large pool per process would
	// multiply connections against a shared UAT box for no benefit.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	r.pools[key] = db
	return db, t, nil
}

// Close releases every pooled connection.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for k, db := range r.pools {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(r.pools, k)
	}
	return firstErr
}

func urlEscape(s string) string { return url.QueryEscape(s) }
