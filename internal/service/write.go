package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/codex-k8s/hrm-sql-mcp/internal/approver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/audit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
	"github.com/codex-k8s/hrm-sql-mcp/internal/tsql"
)

// ErrApprovalRequired is returned when a commit needs a human decision that
// has not been made yet. It carries the id to approve.
type ErrApprovalRequired struct {
	ID  string
	Dir string
}

func (e *ErrApprovalRequired) Error() string {
	return fmt.Sprintf("approval required: run `hrm-sql-mcp approve %s` in a terminal "+
		"(pending requests live in %s)", e.ID, e.Dir)
}

// WriteOptions controls one write.
type WriteOptions struct {
	// Commit false runs the statement and rolls it back. See
	// sqlrun.RehearsalCaveats for what that does and does not prove.
	Commit bool
	// ApprovalID is a decision already granted for this exact statement.
	// Empty on the first call: the service raises a request and returns
	// ErrApprovalRequired with the id to approve.
	ApprovalID string
	// Reason is the caller's justification, shown to whoever approves.
	Reason string
}

// WriteResult is the outcome of a write.
type WriteResult struct {
	*sqlrun.ExecResult
	Label      tsql.Label `json:"label"`
	ApprovalID string     `json:"approval_id,omitempty"`
	// SnapshotPath is set by procedure deployments.
	SnapshotPath string `json:"snapshot_path,omitempty"`
}

// Execute runs a statement against the read-write login.
//
// The order is deliberate and is the whole of the write path:
//
//  1. Open read-write. The policy must mark the target writable and a
//     read-write credential must exist; either missing is a refusal.
//  2. Record intent, durably, before anything runs. A process killed after
//     this point leaves evidence of what it was about to do.
//  3. A rehearsal needs no approval — it changes nothing, and requiring one
//     would train people to approve without reading.
//  4. A commit needs an approval bound to this exact statement text.
//  5. Record the outcome.
func (s *Service) Execute(ctx context.Context, alias, statement string, opt WriteOptions) (*WriteResult, *target.Target, error) {
	const tool = "mssql_execute"

	db, t, err := s.openWritable(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}
	login := s.login(ctx, db, t.Alias())
	label := tsql.Classify(statement)

	id := opt.ApprovalID
	if id == "" {
		id = audit.NewID()
	}

	intent := audit.Event{
		Tool: tool, Phase: audit.PhaseIntent, CorrelationID: id,
		Statement: statement, Classification: label.Summary(),
		Committed: opt.Commit,
	}
	if err := s.record(intent, t, login); err != nil {
		return nil, t, err
	}

	if opt.Commit {
		if err := s.requireApproval(ctx, id, tool, statement, label, t, login, opt); err != nil {
			return nil, t, err
		}
	}

	res, execErr := sqlrun.Exec(ctx, db, statement, s.writeLimits(), opt.Commit)

	ev := audit.Event{
		Tool: tool, Phase: audit.PhaseOutcome, CorrelationID: id,
		Statement: statement, Classification: label.Summary(),
		Committed: opt.Commit,
	}
	if opt.Commit {
		ev.Approval = id
	}
	if res != nil {
		ev.RowsAffected = res.RowsAffected
		ev.ElapsedMS = res.ElapsedMS
		ev.Committed = res.Committed
	}
	if err := s.finish(ev, t, login, execErr); err != nil {
		return nil, t, err
	}
	return &WriteResult{ExecResult: res, Label: label, ApprovalID: approvalIDOf(opt)}, t, nil
}

// SPDeploy replaces a stored procedure, saving the previous definition first.
func (s *Service) SPDeploy(ctx context.Context, alias, name, definition string, opt WriteOptions) (*WriteResult, *target.Target, error) {
	const tool = "mssql_sp_deploy"

	db, t, err := s.openWritable(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}
	login := s.login(ctx, db, t.Alias())
	label := tsql.Classify(definition)

	id := opt.ApprovalID
	if id == "" {
		id = audit.NewID()
	}

	// Read the current definition before anything else. If this fails we have
	// not touched the server, and we stop: deploying without knowing what is
	// being replaced is the case the snapshot exists for.
	cctx, cancel := s.catalogContext(ctx)
	existing, getErr := spdb.Get(cctx, db, name)
	cancel()
	prior := ""
	switch {
	case getErr == nil && existing.Encrypted:
		return nil, t, fmt.Errorf(
			"%s is encrypted on the server, so its current definition cannot be saved; refusing to overwrite it", name)
	case getErr == nil:
		prior = existing.Definition
	case !strings.Contains(getErr.Error(), "not found"):
		return nil, t, fmt.Errorf("read the current definition of %s: %w", name, getErr)
	}

	snapPath, err := s.snaps.Save(t.Alias(), t.Database(), name, prior)
	if err != nil {
		return nil, t, err
	}

	intent := audit.Event{
		Tool: tool, Phase: audit.PhaseIntent, CorrelationID: id,
		Statement: definition, Classification: label.Summary(),
		Committed: opt.Commit, SnapshotPath: snapPath,
	}
	if err := s.record(intent, t, login); err != nil {
		return nil, t, err
	}

	if opt.Commit {
		if err := s.requireApproval(ctx, id, tool, definition, label, t, login, opt); err != nil {
			return nil, t, err
		}
	}

	res, execErr := sqlrun.Exec(ctx, db, definition, s.writeLimits(), opt.Commit)

	ev := audit.Event{
		Tool: tool, Phase: audit.PhaseOutcome, CorrelationID: id,
		Statement: definition, Classification: label.Summary(),
		Committed: opt.Commit, SnapshotPath: snapPath,
	}
	if opt.Commit {
		ev.Approval = id
	}
	if res != nil {
		ev.RowsAffected = res.RowsAffected
		ev.ElapsedMS = res.ElapsedMS
		ev.Committed = res.Committed
	}
	if err := s.finish(ev, t, login, execErr); err != nil {
		return nil, t, err
	}
	return &WriteResult{
		ExecResult: res, Label: label,
		ApprovalID: approvalIDOf(opt), SnapshotPath: snapPath,
	}, t, nil
}

// requireApproval raises a request on the first call and waits for a decision
// on the second.
//
// Two calls rather than one long block: an MCP tool call that sat waiting for
// a person would hit the client's timeout, and the agent would have no way to
// report why. Returning "approval required" with an id lets the agent tell the
// user what to run.
func (s *Service) requireApproval(ctx context.Context, id, tool, statement string,
	label tsql.Label, t *target.Target, login string, opt WriteOptions) error {

	if opt.ApprovalID == "" {
		req := approver.Request{
			ID: id, Actor: s.cfg.Actor, Tool: tool,
			Alias: t.Alias(), Server: t.Addr(), Database: t.Database(), Login: login,
			Statement: statement, Summary: label.Summary(), Objects: label.Objects,
			Rehearsal: opt.Reason,
		}
		if err := s.appr.Raise(req); err != nil {
			return err
		}
		_ = s.record(audit.Event{
			Tool: tool, Phase: audit.PhaseOutcome, CorrelationID: id,
			Statement: statement, Classification: label.Summary(),
			Outcome: audit.OutcomeDenied, Error: "awaiting approval",
		}, t, login)
		return &ErrApprovalRequired{ID: id, Dir: s.appr.Dir()}
	}

	// The decision is checked against this exact statement inside Wait. An
	// approval is for one text, not for a session.
	if _, err := s.appr.Wait(ctx, opt.ApprovalID, statement); err != nil {
		_ = s.record(audit.Event{
			Tool: tool, Phase: audit.PhaseOutcome, CorrelationID: id,
			Statement: statement, Classification: label.Summary(),
			Outcome: audit.OutcomeDenied, Error: err.Error(),
		}, t, login)
		return err
	}
	return nil
}

// openWritable opens the read-write connection, refusing early and audibly.
func (s *Service) openWritable(ctx context.Context, alias, tool string) (*sql.DB, *target.Target, error) {
	alias, err := s.ResolveAlias(alias)
	if err != nil {
		s.deny(tool, alias, err)
		return nil, nil, err
	}
	db, t, err := s.reg.Open(ctx, alias, target.ReadWrite)
	if err != nil {
		s.deny(tool, alias, err)
		if errors.Is(err, target.ErrNotWritable) {
			return nil, nil, fmt.Errorf("%w (policy marks %q read-only)", sqlrun.ErrNoWriteCredential, alias)
		}
		return nil, nil, err
	}
	return db, t, nil
}

func approvalIDOf(opt WriteOptions) string {
	if opt.Commit {
		return opt.ApprovalID
	}
	return ""
}
