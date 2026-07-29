package sqlrun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExecResult is the outcome of a write.
type ExecResult struct {
	// RowsAffected is the total across statements in the batch. On a rehearsal
	// it is what the batch *would* have changed, which is the number a person
	// approving it should be looking at.
	RowsAffected int64 `json:"rows_affected"`
	// Committed distinguishes a real write from a rehearsal.
	Committed bool `json:"committed"`
	// ElapsedMS covers the statement, not the approval wait.
	ElapsedMS int64 `json:"elapsed_ms"`
	// Caveats are the ways this result can mislead. Always populated for a
	// rehearsal; see RehearsalCaveats.
	Caveats []string `json:"caveats,omitempty"`
}

// RehearsalCaveats are the three ways a rolled-back rehearsal lies, stated
// every time rather than documented once.
//
// A rehearsal that reports "3 rows affected, rolled back" reads as proof that
// running it for real is safe. It is not, and the gap is not obvious to
// someone who has not thought about transactions for a while — so the caveats
// travel with the result, where the decision is made.
var RehearsalCaveats = []string{
	"IDENTITY and SEQUENCE values consumed by this batch are NOT returned by the rollback — the next real insert will skip them.",
	"Side effects outside the transaction did happen: sp_send_dbmail, xp_cmdshell, linked-server calls and anything writing to the filesystem are not covered by ROLLBACK.",
	"Locks were held for the duration. A rehearsal on a busy table blocks other sessions exactly as the real write would.",
}

// ErrNoWriteCredential is returned when a write is attempted against a target
// the policy did not mark writable, or with no read-write credential.
var ErrNoWriteCredential = errors.New("no read-write access configured for this target")

// Exec runs a statement inside a transaction and either commits it or rolls it
// back.
//
// The rehearsal path (commit=false) exists because "what would this do" is the
// question people actually have before a write, and the only honest way to
// answer it is to do the work and undo it. That it is honest about *rows* does
// not make it honest about everything: see RehearsalCaveats, which this
// attaches to every rehearsal result.
//
// Both paths hold the same locks and take the same timeouts. A rehearsal is
// not a cheap preview — it is the real statement, briefly.
func Exec(ctx context.Context, db *sql.DB, statement string, lim Limits, commit bool) (*ExecResult, error) {
	if strings.TrimSpace(statement) == "" {
		return nil, ErrEmptyStatement
	}
	lim = lim.withDefaults()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	qctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	// The lock timeout matters more here than for reads. A write that blocks
	// is a write holding its own locks while it waits, and the plan's rule is
	// to fail rather than stall a colleague who is mid-test.
	lockMS := int64(-1)
	if lim.LockTimeout > 0 {
		lockMS = lim.LockTimeout.Milliseconds()
	}
	if _, err := conn.ExecContext(qctx, fmt.Sprintf("SET LOCK_TIMEOUT %d", lockMS)); err != nil {
		return nil, wrapErr(qctx, ctx, fmt.Errorf("set lock timeout: %w", err))
	}

	tx, err := conn.BeginTx(qctx, nil)
	if err != nil {
		return nil, wrapErr(qctx, ctx, fmt.Errorf("begin transaction: %w", err))
	}
	// Rollback on every path that is not an explicit commit, including panics.
	// A transaction left open on a pooled connection holds locks until the
	// connection is reused or dropped.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	start := time.Now()
	res, execErr := tx.ExecContext(qctx, statement)
	elapsed := time.Since(start).Milliseconds()
	if execErr != nil {
		return nil, wrapErr(qctx, ctx, execErr)
	}

	out := &ExecResult{ElapsedMS: elapsed}
	// RowsAffected is unsupported by some statement shapes (DDL); that is not
	// an error, it just means there is no count to report.
	if n, err := res.RowsAffected(); err == nil {
		out.RowsAffected = n
	}

	if !commit {
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("rollback: %w", err)
		}
		committed = true // nothing left for the deferred rollback to do
		out.Caveats = append(out.Caveats, RehearsalCaveats...)
		return out, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, wrapErr(qctx, ctx, fmt.Errorf("commit: %w", err))
	}
	committed = true
	out.Committed = true
	return out, nil
}
