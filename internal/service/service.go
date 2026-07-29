package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/approver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/audit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/config"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
	"github.com/codex-k8s/hrm-sql-mcp/internal/snapshots"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// Service holds the process-wide state: one policy, one connection registry,
// one audit log.
type Service struct {
	cfg   config.Config
	pol   *policy.Policy
	reg   *target.Registry
	log   *audit.Writer
	appr  *approver.Store
	snaps *snapshots.Store
	// creds is kept so the targets listing can report whether a login is
	// configured, and from where, without ever reading the secret itself.
	creds *config.Store
	// overrides records what the environment changed about the policy. Shown
	// by the targets listing; see policy.Override for why it is kept at all.
	overrides []policy.Override

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
	cfg, pol, log, overrides, err := newCore()
	if err != nil {
		return nil, err
	}
	src, err := cfg.LoadSource()
	if err != nil {
		log.Close()
		return nil, err
	}
	store := config.NewStore(src)
	reg, err := target.NewRegistry(pol, cfg.Profile, store.Credentials())
	if err != nil {
		log.Close()
		return nil, err
	}
	// Both live beside the audit log and are opened at startup for the same
	// reason it is: a write path that discovers halfway through that it cannot
	// record or cannot save a pre-image has already done the damage.
	appr, err := approver.Open(approvalDir(pol.Audit.SnapshotDir))
	if err != nil {
		log.Close()
		return nil, err
	}
	snaps, err := snapshots.Open(pol.Audit.SnapshotDir)
	if err != nil {
		log.Close()
		return nil, err
	}

	return &Service{
		cfg: cfg, pol: pol, reg: reg, log: log, creds: store,
		appr: appr, snaps: snaps, logins: map[string]string{},
		overrides: overrides,
	}, nil
}

// Overrides reports what the environment changed about the policy file.
func (s *Service) Overrides() []policy.Override { return s.overrides }

// CredentialOrigin says where a target's login was found, without revealing
// it. Empty when none is configured, which is how the targets listing can
// distinguish "no credential" from "credential present but the guard refused".
func (s *Service) CredentialOrigin(alias string, mode target.AccessMode) (string, bool) {
	pt, ok := s.pol.TargetByAlias(alias)
	if !ok {
		return "", false
	}
	return s.creds.Origin(pt.CredentialKey, mode)
}

// NewOffline wires only the parts that need no server: configuration, policy,
// and the audit log.
//
// Credentials are not read and no registry is built, so this succeeds on a
// machine that has never been given a database login. That is the entire
// point — the offline audit exists to run in exactly those places, and a
// constructor that demanded a 0600 credentials file first would make the CI
// lane unusable on any clean checkout.
//
// The returned Service refuses every operation that would connect; see open.
func NewOffline() (*Service, error) {
	cfg, pol, log, overrides, err := newCore()
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg: cfg, pol: pol, log: log,
		logins: map[string]string{}, overrides: overrides,
	}, nil
}

// newCore loads what both constructors need. The audit log is opened here, so
// neither path can reach an operation before there is somewhere to record it.
func newCore() (config.Config, *policy.Policy, *audit.Writer, []policy.Override, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, nil, nil, err
	}
	if cfg.Profile == "" {
		return cfg, nil, nil, nil, fmt.Errorf("HRM_SQL_MCP_PROFILE is not set; it has no default so that " +
			"pointing at an environment is always a deliberate act")
	}
	// The source is read before the policy because it can rewrite it. Loading
	// the policy first and patching it afterwards would mean the validator saw
	// a configuration that no connection uses.
	src, err := cfg.LoadSource()
	if err != nil {
		return cfg, nil, nil, nil, err
	}
	pol, overrides, err := policy.LoadWithOverrides(cfg.PolicyPath, src)
	if err != nil {
		return cfg, nil, nil, nil, err
	}
	// The profile check normally lives in NewRegistry, but the offline path
	// never builds one. Repeating it here keeps "policy says uat, environment
	// says local" a refusal on both paths rather than only the connecting one.
	if pol.Profile != cfg.Profile {
		return cfg, nil, nil, nil, fmt.Errorf(
			"profile mismatch: policy declares %q but HRM_SQL_MCP_PROFILE is %q",
			pol.Profile, cfg.Profile)
	}
	log, err := audit.Open(pol.Audit.File)
	if err != nil {
		return cfg, nil, nil, nil, err
	}
	return cfg, pol, log, overrides, nil
}

// Close releases connections and the audit log.
func (s *Service) Close() error {
	var err error
	if s.reg != nil {
		err = s.reg.Close()
	}
	if cerr := s.log.Close(); err == nil {
		err = cerr
	}
	return err
}

// Policy returns the loaded policy.
func (s *Service) Policy() *policy.Policy { return s.pol }

// Aliases lists the declared target aliases. Read from the policy rather than
// the registry so it still answers on an offline Service, which has none.
func (s *Service) Aliases() []string {
	out := make([]string, 0, len(s.pol.Targets))
	for _, t := range s.pol.Targets {
		out = append(out, t.Alias)
	}
	return out
}

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
	// An offline Service has no registry at all. Refusing here rather than
	// dereferencing nil means the one thing this Service cannot do fails with
	// a sentence explaining why, on every operation that tries it.
	if s.reg == nil {
		err := fmt.Errorf("this process was started without credentials (offline); %s needs a database", tool)
		s.deny(tool, alias, err)
		return nil, nil, err
	}
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

// record writes one audit line, filling in the target fields every record
// shares. Used for intent records, which have no outcome yet.
func (s *Service) record(ev audit.Event, t *target.Target, login string) error {
	ev.Actor = s.cfg.Actor
	if t != nil {
		ev.Alias = t.Alias()
		ev.Server = t.Addr()
		ev.Database = t.Database()
		ev.Mode = t.Mode().String()
		ev.Login = login
	}
	if ev.Outcome == "" {
		ev.Outcome = audit.OutcomeOK
	}
	if err := s.log.Write(ev); err != nil {
		return fmt.Errorf("audit (%s): %w", s.log.Path(), err)
	}
	return nil
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

// approvalDir puts pending approvals beside the snapshots rather than in the
// same directory: a person listing pending requests should not have to read
// past procedure bodies to find them.
func approvalDir(snapshotDir string) string {
	if strings.TrimSpace(snapshotDir) == "" {
		return "~/.local/state/hrm-sql-mcp/approvals"
	}
	return filepath.Join(filepath.Dir(snapshotDir), "approvals")
}

// Approvals exposes the store so the approve subcommand can reach it.
func (s *Service) Approvals() *approver.Store { return s.appr }

// Snapshots exposes the pre-image store.
func (s *Service) Snapshots() *snapshots.Store { return s.snaps }

// writeLimits bounds a write. ExecuteTimeout rather than QueryTimeout: a
// deployment legitimately takes longer than a select, and the lock timeout is
// what protects other sessions in the meantime.
func (s *Service) writeLimits() sqlrun.Limits {
	return sqlrun.Limits{
		MaxRows:     s.pol.Limits.MaxRows,
		MaxBytes:    s.pol.Limits.MaxBytes,
		Timeout:     s.pol.Limits.ExecuteTimeout,
		LockTimeout: s.pol.Limits.LockTimeout,
	}
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
