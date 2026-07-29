package sqlrun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// Limits bound one call. Zero fields take the defaults below; a negative
// LockTimeout means "wait forever", matching SET LOCK_TIMEOUT -1.
type Limits struct {
	MaxRows     int
	MaxBytes    int
	Timeout     time.Duration
	LockTimeout time.Duration
}

func (l Limits) withDefaults() Limits {
	if l.MaxRows <= 0 {
		l.MaxRows = 500
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 1 << 20
	}
	if l.Timeout <= 0 {
		l.Timeout = 30 * time.Second
	}
	if l.LockTimeout == 0 {
		l.LockTimeout = 5 * time.Second
	}
	return l
}

// Truncation reasons, reported so a caller can tell "that is all of it" from
// "that is as much as you were allowed to see".
const (
	TruncatedByRows  = "max_rows"
	TruncatedByBytes = "max_bytes"
)

// Column is one column of a result set.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Set is one result set. A batch may produce several.
type Set struct {
	Columns     []Column `json:"columns"`
	Rows        [][]any  `json:"rows"`
	Truncated   bool     `json:"truncated"`
	TruncatedBy string   `json:"truncated_by,omitempty"`
}

// Result is the outcome of one Query call.
type Result struct {
	Sets []Set `json:"sets"`
	// Bytes is the estimated size of the returned values.
	Bytes int `json:"bytes"`
	// ElapsedMS covers the query only, not connection acquisition.
	ElapsedMS int64 `json:"elapsed_ms"`
	// Truncated is true when any set was cut short.
	Truncated bool `json:"truncated"`
}

// ErrEmptyStatement is returned for whitespace-only SQL, which is far more
// often a bug in the caller than a request to do nothing.
var ErrEmptyStatement = errors.New("empty statement")

// ErrTimeout wraps a deadline that the query hit.
var ErrTimeout = errors.New("query timed out")

// Query runs a statement and returns bounded results.
//
// The connection is checked out for the whole call rather than each statement
// borrowing from the pool. SET LOCK_TIMEOUT is session state, and
// database/sql resets a session when it is handed back and picked up again —
// so setting it on a borrowed connection and running the query on the next one
// would leave the setting silently inert, which is the worst outcome: a guard
// that reports success and does nothing.
func Query(ctx context.Context, db *sql.DB, statement string, args []any, lim Limits) (*Result, error) {
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

	// A literal is required here; SET LOCK_TIMEOUT rejects a parameter. The
	// value is an int64 we computed, so there is no injection surface.
	lockMS := int64(-1)
	if lim.LockTimeout > 0 {
		lockMS = lim.LockTimeout.Milliseconds()
	}
	if _, err := conn.ExecContext(qctx, fmt.Sprintf("SET LOCK_TIMEOUT %d", lockMS)); err != nil {
		return nil, wrapErr(qctx, ctx, fmt.Errorf("set lock timeout: %w", err))
	}

	start := time.Now()
	rows, err := conn.QueryContext(qctx, statement, args...)
	if err != nil {
		return nil, wrapErr(qctx, ctx, err)
	}
	defer rows.Close()

	res := &Result{}
	for {
		set, stop, err := readSet(rows, lim, &res.Bytes)
		if err != nil {
			return nil, wrapErr(qctx, ctx, err)
		}
		res.Sets = append(res.Sets, set)
		if set.Truncated {
			res.Truncated = true
		}
		// Once a cap is hit, stop reading entirely. Closing the rows tells the
		// server to stop producing them too, rather than having it finish a
		// result nobody will look at.
		if stop || !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil && !res.Truncated {
		return nil, wrapErr(qctx, ctx, err)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// readSet consumes one result set, stopping at the row or byte cap.
//
// stop is true when a cap was hit, which is a different thing from
// set.Truncated only in that the caller uses it to abandon later result sets
// as well.
func readSet(rows *sql.Rows, lim Limits, total *int) (set Set, stop bool, err error) {
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return set, false, err
	}
	set.Columns = make([]Column, len(colTypes))
	for i, ct := range colTypes {
		set.Columns[i] = Column{Name: ct.Name(), Type: ct.DatabaseTypeName()}
	}

	vals := make([]any, len(colTypes))
	ptrs := make([]any, len(colTypes))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	for rows.Next() {
		if len(set.Rows) >= lim.MaxRows {
			set.Truncated, set.TruncatedBy = true, TruncatedByRows
			return set, true, nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return set, false, err
		}

		row := make([]any, len(vals))
		size := 0
		for i := range vals {
			row[i] = convert(vals[i], set.Columns[i].Type)
			size += sizeOf(row[i])
		}
		// Always keep the first row even if it alone exceeds the cap: an empty
		// result with a "too big" flag reads as "nothing matched", and the
		// caller cannot tell what it is up against without seeing one row.
		if *total+size > lim.MaxBytes && len(set.Rows) > 0 {
			set.Truncated, set.TruncatedBy = true, TruncatedByBytes
			return set, true, nil
		}
		*total += size
		set.Rows = append(set.Rows, row)
	}
	return set, false, rows.Err()
}

// wrapErr turns an expired deadline into ErrTimeout, distinguishing our own
// limit from the caller cancelling.
//
// Both contexts are needed: qctx is always Done once the parent is, so
// checking it alone would report our timeout for a user's Ctrl-C.
func wrapErr(qctx, parent context.Context, err error) error {
	if err == nil {
		return nil
	}
	if parent.Err() == nil && errors.Is(qctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return err
}

// ServerErrorNumber reports the SQL Server error number behind err.
//
// Tests use this to prove that a refusal came from the server's permission
// system (229) rather than from anything on this side. The distinction is the
// whole point: a client-side rejection would look identical to the user while
// meaning the DENY was never checked.
func ServerErrorNumber(err error) (int32, bool) {
	var se mssql.Error
	if errors.As(err, &se) {
		return se.Number, true
	}
	return 0, false
}
