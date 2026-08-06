package sqlrun_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

// TestLockTimeoutReachesTheSession is the proof that SET LOCK_TIMEOUT is not
// inert.
//
// The failure this guards against is invisible from the outside: if the SET
// ran on one pooled connection and the query on another, everything would
// still work and queries would still wait forever behind a lock. Asking the
// server what it thinks @@LOCK_TIMEOUT is, from inside the very call that set
// it, is the only way to tell the two apart.
func TestLockTimeoutReachesTheSession(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	res, err := sqlrun.Query(ctx, db, "SELECT @@LOCK_TIMEOUT", nil,
		sqlrun.Limits{LockTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Sets[0].Rows[0][0]; got != int64(5000) {
		t.Errorf("@@LOCK_TIMEOUT = %v, want 5000 — the setting did not reach the query's session", got)
	}

	// Negative means "wait forever" and must be passed through, not defaulted.
	res, err = sqlrun.Query(ctx, db, "SELECT @@LOCK_TIMEOUT", nil,
		sqlrun.Limits{LockTimeout: -1})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := res.Sets[0].Rows[0][0]; got != int64(-1) {
		t.Errorf("@@LOCK_TIMEOUT = %v, want -1", got)
	}
}

// TestTimeoutCancelsTheServerToo covers verification item 6 of the plan: a one
// minute WAITFOR under a five second limit must come back in about five
// seconds, and must not leave the server working on a request nobody is
// waiting for.
func TestTimeoutCancelsTheServerToo(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	start := time.Now()
	_, err := sqlrun.Query(ctx, db, "WAITFOR DELAY '00:01:00'", nil,
		sqlrun.Limits{Timeout: 5 * time.Second})
	elapsed := time.Since(start)

	if !errors.Is(err, sqlrun.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("took %v; the deadline did not interrupt the query", elapsed)
	}

	// The pool must still be usable afterwards. A cancelled query that poisons
	// the connection would turn one slow statement into a dead tool.
	res, err := sqlrun.Query(ctx, db, "SELECT 1", nil, sqlrun.Limits{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("follow-up query failed after a cancellation: %v", err)
	}
	if res.Sets[0].Rows[0][0] != int64(1) {
		t.Errorf("follow-up returned %v", res.Sets[0].Rows[0][0])
	}

	// And nothing of ours should still be running. The read-only login sees
	// only its own sessions here, which is exactly the scope we care about.
	res, err = sqlrun.Query(ctx, db,
		"SELECT COUNT(*) FROM sys.dm_exec_requests r "+
			"JOIN sys.dm_exec_sessions s ON s.session_id = r.session_id "+
			"WHERE s.program_name = 'hrm-sql-mcp' AND r.session_id <> @@SPID", nil,
		sqlrun.Limits{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("check for leftover requests: %v", err)
	}
	if n := res.Sets[0].Rows[0][0]; n != int64(0) {
		t.Errorf("%v of our requests are still running after the timeout", n)
	}
}

// TestReadOnlyLoginCannotWrite is verification item 2. The assertion is on the
// error *number*, not on the fact that it failed: a client-side refusal would
// also make the call fail, while proving nothing about the login's permissions.
func TestReadOnlyLoginCannotWrite(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	res, err := sqlrun.Query(ctx, db,
		"SELECT TOP 1 TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES "+
			"WHERE TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatalf("find a table: %v", err)
	}
	if len(res.Sets[0].Rows) == 0 {
		t.Skip("no base tables in this database")
	}
	schema := res.Sets[0].Rows[0][0].(string)
	table := res.Sets[0].Rows[0][1].(string)

	// WHERE 1=0 so that a missing DENY does not become a deleted row.
	_, err = sqlrun.Query(ctx, db,
		"DELETE FROM ["+schema+"].["+table+"] WHERE 1 = 0", nil, sqlrun.Limits{})
	if err == nil {
		t.Fatalf("DELETE on %s.%s succeeded; the read-only login can write", schema, table)
	}
	n, ok := sqlrun.ServerErrorNumber(err)
	if !ok {
		t.Fatalf("refusal did not come from SQL Server: %v", err)
	}
	if n != 229 {
		t.Errorf("SQL Server error %d, want 229 (permission denied): %v", n, err)
	}
}

func TestTruncation(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()
	const q = "SELECT TOP 1000 name, object_id FROM sys.all_objects ORDER BY object_id"

	t.Run("by rows", func(t *testing.T) {
		res, err := sqlrun.Query(ctx, db, q, nil, sqlrun.Limits{MaxRows: 10})
		if err != nil {
			t.Fatal(err)
		}
		set := res.Sets[0]
		if len(set.Rows) != 10 {
			t.Errorf("got %d rows, want 10", len(set.Rows))
		}
		if !set.Truncated || set.TruncatedBy != sqlrun.TruncatedByRows {
			t.Errorf("truncated=%v by=%q", set.Truncated, set.TruncatedBy)
		}
	})

	t.Run("by bytes", func(t *testing.T) {
		res, err := sqlrun.Query(ctx, db, q, nil, sqlrun.Limits{MaxBytes: 200})
		if err != nil {
			t.Fatal(err)
		}
		set := res.Sets[0]
		if !set.Truncated || set.TruncatedBy != sqlrun.TruncatedByBytes {
			t.Errorf("truncated=%v by=%q", set.Truncated, set.TruncatedBy)
		}
		if len(set.Rows) == 0 {
			t.Error("no rows returned; the first row must survive the byte cap")
		}
	})

	t.Run("no truncation when it fits", func(t *testing.T) {
		res, err := sqlrun.Query(ctx, db, "SELECT TOP 3 name FROM sys.all_objects", nil, sqlrun.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Truncated {
			t.Error("reported truncation for three rows")
		}
	})
}

func TestValuesRoundTrip(t *testing.T) {
	db, _ := testenv.Open(t)
	res, err := sqlrun.Query(context.Background(), db, `
SELECT CAST(123456789012345678.99 AS decimal(20,2))        AS d,
       CAST(0xDEADBEEF AS varbinary(4))                     AS b,
       CAST('01234567-89AB-CDEF-0123-456789ABCDEF' AS uniqueidentifier) AS g,
       N'中文薪資'                                          AS n,
       CAST('2026-07-29 13:45:30.123' AS datetime2(3))      AS t`, nil, sqlrun.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	row := res.Sets[0].Rows[0]
	want := []any{
		"123456789012345678.99",
		"0xDEADBEEF",
		"01234567-89AB-CDEF-0123-456789ABCDEF",
		"中文薪資",
		"2026-07-29T13:45:30.123",
	}
	for i, w := range want {
		if row[i] != w {
			t.Errorf("column %d (%s) = %#v, want %#v", i, res.Sets[0].Columns[i].Type, row[i], w)
		}
	}
}

// TestMultipleResultSets guards against silently dropping everything after the
// first SELECT — the sort of loss a caller cannot detect from the output.
func TestMultipleResultSets(t *testing.T) {
	db, _ := testenv.Open(t)
	res, err := sqlrun.Query(context.Background(), db, "SELECT 1 AS a; SELECT 2 AS b, 3 AS c", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sets) != 2 {
		t.Fatalf("got %d result sets, want 2", len(res.Sets))
	}
	if len(res.Sets[1].Columns) != 2 {
		t.Errorf("second set has %d columns, want 2", len(res.Sets[1].Columns))
	}
}

func TestEmptyStatementIsRejected(t *testing.T) {
	db, _ := testenv.Open(t)
	if _, err := sqlrun.Query(context.Background(), db, "   \n\t ", nil, sqlrun.Limits{}); !errors.Is(err, sqlrun.ErrEmptyStatement) {
		t.Errorf("err = %v, want ErrEmptyStatement", err)
	}
}

// TestExplainDoesNotExecute is the property the whole tool rests on. If
// SHOWPLAN were ever not in effect, this statement would run — so it is
// written to be observable: a WAITFOR that would take a minute returns
// immediately when only planned.
func TestExplainDoesNotExecute(t *testing.T) {
	db, _ := testenv.Open(t)

	start := time.Now()
	plan, err := sqlrun.Explain(context.Background(), db, "WAITFOR DELAY '00:01:00'", sqlrun.Limits{}, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v — the statement was executed, not just planned", elapsed)
	}
	if plan.XMLBytes == 0 {
		t.Error("no plan returned")
	}
}

// TestExplainLeavesTheConnectionUsable guards the failure that would be
// hardest to notice: a pooled connection handed back still in SHOWPLAN mode
// would make the next caller's query return a plan instead of their data.
func TestExplainLeavesTheConnectionUsable(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	if _, err := sqlrun.Explain(ctx, db, "SELECT TOP 5 name FROM sys.all_objects", sqlrun.Limits{}, false); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Hammer the pool so the explain connection is very likely reused.
	for i := range 5 {
		res, err := sqlrun.Query(ctx, db, "SELECT 42 AS n", nil, sqlrun.Limits{})
		if err != nil {
			t.Fatalf("query %d after explain: %v", i, err)
		}
		if got := res.Sets[0].Rows[0][0]; got != int64(42) {
			t.Fatalf("query %d returned %#v, want 42 — the connection was still in SHOWPLAN mode", i, got)
		}
	}
}

func TestExplainRealPlan(t *testing.T) {
	db, _ := testenv.Open(t)
	plan, err := sqlrun.Explain(context.Background(), db,
		"SELECT o.name, c.name FROM sys.objects o JOIN sys.columns c ON c.object_id = o.object_id",
		sqlrun.Limits{}, true)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(plan.Statements) == 0 {
		t.Fatal("no statements in the plan")
	}
	if plan.TotalCost() <= 0 {
		t.Errorf("total cost = %v, want a positive estimate", plan.TotalCost())
	}
	// A join has to produce more than one operator; one would mean the nested
	// RelOps were not being walked.
	if len(plan.Operators) < 2 {
		t.Errorf("got %d operators for a join", len(plan.Operators))
	}
	if plan.XML == "" {
		t.Error("include_xml was set but no XML came back")
	}
}

func TestExplainRejectsGarbage(t *testing.T) {
	db, _ := testenv.Open(t)
	// Compilation still happens, so an invalid statement must fail rather than
	// return an empty plan that reads as "this is fine".
	if _, err := sqlrun.Explain(context.Background(), db,
		"SELECT * FROM this_table_does_not_exist_0000", sqlrun.Limits{}, false); err == nil {
		t.Error("explaining a statement against a missing table returned no error")
	}
}

// TestExecRehearsalDoesNotPersist is the plan's verification item 5, and the
// property the whole rehearsal idea rests on: rows_affected reports real work,
// and afterwards nothing changed.
func TestExecRehearsalDoesNotPersist(t *testing.T) {
	db := testenv.OpenWritable(t)
	ctx := context.Background()

	// Pick a real row and remember its value.
	res, err := sqlrun.Query(ctx, db,
		"SELECT TOP 1 emp_no, ISNULL(remark, '') FROM ADVANCE_BONUS_GRANT ORDER BY emp_no", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatalf("read a row: %v", err)
	}
	if len(res.Sets[0].Rows) == 0 {
		t.Skip("no rows in ADVANCE_BONUS_GRANT")
	}
	empNo := res.Sets[0].Rows[0][0].(string)
	before := res.Sets[0].Rows[0][1].(string)

	const marker = "REHEARSAL-SHOULD-NOT-PERSIST"
	stmt := "UPDATE ADVANCE_BONUS_GRANT SET remark = '" + marker + "' WHERE emp_no = '" + empNo + "'"

	er, err := sqlrun.Exec(ctx, db, stmt, sqlrun.Limits{}, false)
	if err != nil {
		t.Fatalf("Exec rehearsal: %v", err)
	}
	if er.RowsAffected < 1 {
		t.Errorf("rows_affected = %d; a rehearsal must report the work it would do", er.RowsAffected)
	}
	if er.Committed {
		t.Error("a rehearsal reported itself as committed")
	}
	// The caveats are what stop "rolled back" being read as "safe".
	if len(er.Caveats) != len(sqlrun.RehearsalCaveats) {
		t.Errorf("rehearsal returned %d caveats, want %d", len(er.Caveats), len(sqlrun.RehearsalCaveats))
	}

	after, err := sqlrun.Query(ctx, db,
		"SELECT ISNULL(remark, '') FROM ADVANCE_BONUS_GRANT WHERE emp_no = '"+empNo+"'", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatalf("re-read the row: %v", err)
	}
	got := after.Sets[0].Rows[0][0].(string)
	if got == marker {
		t.Fatalf("the rehearsal persisted: remark is now %q", got)
	}
	if got != before {
		t.Errorf("the row changed anyway: %q -> %q", before, got)
	}
}

// TestExecCommitPersistsThenRestore proves the other half. Without it, a
// rehearsal that silently did nothing would also pass the test above.
func TestExecCommitPersistsThenRestore(t *testing.T) {
	db := testenv.OpenWritable(t)
	ctx := context.Background()

	res, err := sqlrun.Query(ctx, db,
		"SELECT TOP 1 emp_no, ISNULL(remark, '') FROM ADVANCE_BONUS_GRANT ORDER BY emp_no", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sets[0].Rows) == 0 {
		t.Skip("no rows in ADVANCE_BONUS_GRANT")
	}
	empNo := res.Sets[0].Rows[0][0].(string)
	before := res.Sets[0].Rows[0][1].(string)

	const marker = "COMMIT-PROOF"
	restore := func() {
		stmt := "UPDATE ADVANCE_BONUS_GRANT SET remark = '" + before + "' WHERE emp_no = '" + empNo + "'"
		if _, err := sqlrun.Exec(context.Background(), db, stmt, sqlrun.Limits{}, true); err != nil {
			t.Errorf("could not restore %s: %v", empNo, err)
		}
	}
	t.Cleanup(restore)

	er, err := sqlrun.Exec(ctx, db,
		"UPDATE ADVANCE_BONUS_GRANT SET remark = '"+marker+"' WHERE emp_no = '"+empNo+"'",
		sqlrun.Limits{}, true)
	if err != nil {
		t.Fatalf("Exec commit: %v", err)
	}
	if !er.Committed || er.RowsAffected < 1 {
		t.Fatalf("commit result = %+v", er)
	}
	if len(er.Caveats) != 0 {
		t.Errorf("a committed write carries rehearsal caveats: %v", er.Caveats)
	}

	after, err := sqlrun.Query(ctx, db,
		"SELECT remark FROM ADVANCE_BONUS_GRANT WHERE emp_no = '"+empNo+"'", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Sets[0].Rows[0][0].(string); got != marker {
		t.Errorf("commit did not persist: remark = %q", got)
	}
}

// TestExecReadOnlyLoginStillCannotWrite: the write path must not become a way
// around the read-only login. Exec on the ro connection is refused by the
// server, not by anything here.
func TestExecReadOnlyLoginStillCannotWrite(t *testing.T) {
	db, _ := testenv.Open(t)
	_, err := sqlrun.Exec(context.Background(), db,
		"UPDATE ADVANCE_BONUS_GRANT SET remark = 'x' WHERE 1 = 0", sqlrun.Limits{}, true)
	if err == nil {
		t.Fatal("Exec succeeded on the read-only connection")
	}
	n, ok := sqlrun.ServerErrorNumber(err)
	if !ok || n != 229 {
		t.Errorf("err = %v (sql error %d, ok=%v), want server error 229", err, n, ok)
	}
}

// TestEmptyResultSetMarshalsAsArray: a query that legitimately matches nothing
// must serialize rows as [], never null.
//
// This is a regression test for a bug whose whole cost was in its error
// message. Rows was filled only by append, so a zero-row result left it nil,
// and a nil slice marshals to null. The MCP tool output is validated against a
// schema generated from these structs, where rows is an array, so the response
// was rejected with:
//
//	validating /properties/sets/items/properties/rows:
//	type: <invalid reflect.Value> has type "null", want "array"
//
// Nothing was wrong with the tool or the query. But that message says the tool
// is broken, and "no rows" is one of the most common answers a query can give
// — so the wrong diagnosis was also the frequent one.
//
// Asserting on the marshalled JSON rather than on Rows != nil is deliberate:
// null vs [] is the thing that broke, and a future refactor could reintroduce
// it (a custom MarshalJSON, a rebuilt struct) while leaving Rows non-nil here.
func TestEmptyResultSetMarshalsAsArray(t *testing.T) {
	db, _ := testenv.Open(t)

	res, err := sqlrun.Query(context.Background(), db,
		"SELECT name FROM sys.tables WHERE name = '<<no such table>>'", nil, sqlrun.Limits{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Sets) != 1 {
		t.Fatalf("got %d result sets, want 1", len(res.Sets))
	}
	if n := len(res.Sets[0].Rows); n != 0 {
		t.Fatalf("got %d rows, want 0 — the query is not testing what it should", n)
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(b, []byte(`"rows":null`)) {
		t.Errorf(`marshalled to "rows":null, want "rows":[] — got %s`, b)
	}
	if !bytes.Contains(b, []byte(`"rows":[]`)) {
		t.Errorf(`missing "rows":[] — got %s`, b)
	}
}
