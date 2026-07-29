package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/audit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/config"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// Service holds the process-wide state: one policy, one connection registry,
// one audit log.
type Service struct {
	cfg config.Config
	pol *policy.Policy
	reg *target.Registry
	log *audit.Writer

	mu     sync.Mutex
	logins map[string]string

	// dict caches the parsed data dictionary. It is 10,821 lines and does not
	// change while the process runs, so parsing it per search would be pure
	// waste in the long-lived MCP case.
	dict dictCache
}

// New wires configuration, policy, credentials and the audit log together.
//
// The audit log is opened before anything can reach a database. Opening it
// afterwards would leave a window where a query could run with nowhere to
// record it, and a window that small is exactly the one nobody tests.
func New() (*Service, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Profile == "" {
		return nil, fmt.Errorf("HRM_SQL_MCP_PROFILE is not set; it has no default so that " +
			"pointing at an environment is always a deliberate act")
	}
	pol, err := policy.Load(cfg.PolicyPath)
	if err != nil {
		return nil, err
	}
	log, err := audit.Open(pol.Audit.File)
	if err != nil {
		return nil, err
	}
	store, err := config.LoadCredentials(cfg.CredentialsPath)
	if err != nil {
		log.Close()
		return nil, err
	}
	reg, err := target.NewRegistry(pol, cfg.Profile, store.Credentials())
	if err != nil {
		log.Close()
		return nil, err
	}
	return &Service{cfg: cfg, pol: pol, reg: reg, log: log, logins: map[string]string{}}, nil
}

// Close releases connections and the audit log.
func (s *Service) Close() error {
	err := s.reg.Close()
	if cerr := s.log.Close(); err == nil {
		err = cerr
	}
	return err
}

// Policy returns the loaded policy.
func (s *Service) Policy() *policy.Policy { return s.pol }

// Aliases lists the declared target aliases.
func (s *Service) Aliases() []string { return s.reg.Aliases() }

// AuditPath returns where records are being written, for startup messages.
func (s *Service) AuditPath() string { return s.log.Path() }

// ResolveAlias fills in the target when there is exactly one.
//
// With several declared it returns an error rather than picking. Guessing
// which database somebody meant is the mistake the whole guard exists to
// prevent, and a convenient default is how that guess gets made.
func (s *Service) ResolveAlias(alias string) (string, error) {
	if alias != "" {
		return alias, nil
	}
	if len(s.pol.Targets) == 1 {
		return s.pol.Targets[0].Alias, nil
	}
	return "", fmt.Errorf("target is required; declared targets: %v", s.Aliases())
}

// ProjectPath resolves a policy-relative path against the project root.
func (s *Service) ProjectPath(rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(s.cfg.ProjectRoot, rel)
}

// open resolves an alias and opens a read-only connection, recording a
// refusal if the guard or the credentials say no.
//
// Refusals are recorded because they are the interesting half of an access
// log: a run of them is either a broken configuration or somebody testing
// where the fence is, and neither is visible if only successes are kept.
func (s *Service) open(ctx context.Context, alias, tool string) (*sql.DB, *target.Target, error) {
	alias, err := s.ResolveAlias(alias)
	if err != nil {
		s.deny(tool, alias, err)
		return nil, nil, err
	}
	db, t, err := s.reg.Open(ctx, alias, target.ReadOnly)
	if err != nil {
		s.deny(tool, alias, err)
		return nil, nil, err
	}
	return db, t, nil
}

func (s *Service) deny(tool, alias string, cause error) {
	// A failure to record a refusal cannot itself be surfaced — the caller is
	// already returning an error, and replacing it with an audit error would
	// hide why the operation was refused.
	_ = s.log.Write(audit.Event{
		Actor: s.cfg.Actor, Tool: tool, Alias: alias,
		Outcome: audit.OutcomeDenied, Error: cause.Error(),
	})
}

// login asks the server which login this connection is using, and caches it.
//
// Cached because every audit record wants it and it costs a round trip, and
// because it cannot change for the life of a pooled target.
func (s *Service) login(ctx context.Context, db *sql.DB, alias string) string {
	s.mu.Lock()
	if name, ok := s.logins[alias]; ok {
		s.mu.Unlock()
		return name
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	name := "unknown"
	if err := db.QueryRowContext(ctx, "SELECT SUSER_SNAME()").Scan(&name); err != nil {
		return "unknown"
	}

	s.mu.Lock()
	s.logins[alias] = name
	s.mu.Unlock()
	return name
}

// Describe returns the envelope a response should carry: which server,
// database and login answered.
//
// The login comes from the cache populated by the operation that just ran, so
// this costs nothing and cannot disagree with what was audited.
func (s *Service) Describe(t *target.Target) map[string]string {
	s.mu.Lock()
	login := s.logins[t.Alias()]
	s.mu.Unlock()
	return t.Describe(login)
}

// finish records the outcome of an operation and returns the error the caller
// should surface.
//
// An audit failure takes precedence over the operation's own error. That reads
// as harsh when the query had already failed, but the alternative is a broken
// audit log staying invisible for as long as queries keep failing for other
// reasons.
func (s *Service) finish(ev audit.Event, t *target.Target, login string, opErr error) error {
	ev.Actor = s.cfg.Actor
	if t != nil {
		ev.Alias = t.Alias()
		ev.Server = t.Addr()
		ev.Database = t.Database()
		ev.Mode = t.Mode().String()
		ev.Login = login
	}
	if opErr != nil {
		ev.Outcome = audit.OutcomeError
		ev.Error = opErr.Error()
		if n, ok := sqlrun.ServerErrorNumber(opErr); ok {
			ev.ServerError = n
		}
	} else if ev.Outcome == "" {
		ev.Outcome = audit.OutcomeOK
	}
	if err := s.log.Write(ev); err != nil {
		return fmt.Errorf("audit (%s): %w", s.log.Path(), err)
	}
	return opErr
}

// queryLimits converts the policy's limits into the ones sqlrun applies.
func (s *Service) queryLimits() sqlrun.Limits {
	return sqlrun.Limits{
		MaxRows:     s.pol.Limits.MaxRows,
		MaxBytes:    s.pol.Limits.MaxBytes,
		Timeout:     s.pol.Limits.QueryTimeout,
		LockTimeout: s.pol.Limits.LockTimeout,
	}
}

// catalogContext bounds the metadata reads (sys.procedures and friends).
func (s *Service) catalogContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.pol.Limits.QueryTimeout)
}
