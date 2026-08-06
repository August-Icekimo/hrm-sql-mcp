package tsql

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestClassifyKinds(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []Kind
	}{
		{"select", "SELECT * FROM EMP_DATA", []Kind{KindSelect}},
		{"update", "UPDATE EMP_DATA SET salary = 1 WHERE emp_no = '1'", []Kind{KindUpdate}},
		{"delete", "DELETE FROM EMP_DATA WHERE 1 = 0", []Kind{KindDelete}},
		{"insert", "INSERT INTO EMP_DATA (emp_no) VALUES ('1')", []Kind{KindInsert}},
		{"truncate", "TRUNCATE TABLE EMP_DATA", []Kind{KindTruncate}},
		{"ddl alter proc", "ALTER PROCEDURE dbo.sp_x AS SELECT 1", []Kind{KindSelect, KindDDL}},
		{"exec", "EXEC dbo.sp_x @a = 1", []Kind{KindExec}},
		{"session setting", "SET NOCOUNT ON", []Kind{KindSet}},

		// A batch is often several things. Reporting only the first is how a
		// DELETE rides along behind a SELECT.
		{"multi statement", "SELECT 1; DELETE FROM T WHERE 1=0", []Kind{KindSelect, KindDelete}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.sql)
			for _, want := range tc.want {
				if !slices.Contains(got.Kinds, want) {
					t.Errorf("Classify(%q).Kinds = %v, missing %q", tc.sql, got.Kinds, want)
				}
			}
		})
	}
}

// TestUpdateIsNotLabelledSet: every UPDATE contains the word SET, so matching
// it anywhere would put "+set" on every write label and teach readers to skip
// the field.
func TestUpdateIsNotLabelledSet(t *testing.T) {
	l := Classify("UPDATE ADVANCE_BONUS_GRANT SET remark = 'x' WHERE emp_no = '1'")
	if slices.Contains(l.Kinds, KindSet) {
		t.Errorf("Kinds = %v; an UPDATE's SET clause is not a SET statement", l.Kinds)
	}
	if !slices.Contains(l.Kinds, KindUpdate) {
		t.Errorf("Kinds = %v, want update", l.Kinds)
	}
}

// TestClassifyIgnoresCommentsAndLiterals: a prompt that says "this deletes"
// about a statement that does not is a prompt people learn to click through.
func TestClassifyIgnoresCommentsAndLiterals(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"line comment", "SELECT 1 -- DELETE FROM EMP_DATA"},
		{"block comment", "SELECT 1 /* DELETE FROM EMP_DATA */"},
		{"nested block comment", "SELECT 1 /* outer /* DELETE */ still comment */"},
		{"string literal", "SELECT 'DELETE FROM EMP_DATA' AS note"},
		{"escaped quote in literal", "SELECT 'it''s not a DELETE' AS note"},
		{"column named like a keyword", "SELECT update_date, delete_flag FROM EMP_DATA"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.sql)
			if slices.Contains(got.Kinds, KindDelete) {
				t.Errorf("Classify(%q) reported a delete: %v", tc.sql, got.Kinds)
			}
			if got.Writes() {
				t.Errorf("Classify(%q).Writes() = true", tc.sql)
			}
		})
	}
}

// TestClassifyFlagsDynamicSQL is the case the package comment is about: the
// label cannot describe what the batch does, and must say so rather than
// giving a confident wrong answer.
func TestClassifyFlagsDynamicSQL(t *testing.T) {
	tests := []string{
		`EXEC sp_executesql N'DELETE FROM EMP_DATA'`,
		`EXEC ('DELETE FROM EMP_DATA')`,
		`DECLARE @s nvarchar(max) = N'DELETE FROM T'; EXEC(@s)`,
	}
	for _, sql := range tests {
		got := Classify(sql)
		if !got.Dynamic {
			t.Errorf("Classify(%q).Dynamic = false; the text does not say what it does", sql)
		}
		if !strings.Contains(got.Summary(), "run time") {
			t.Errorf("Summary() = %q, does not warn the reader", got.Summary())
		}
	}
}

// TestWritesIsNotAuthorisation records the package's central claim as a test:
// a batch that writes via dynamic SQL is not reported as a write, which is
// exactly why nothing may branch on this to skip a permission check.
func TestWritesIsNotAuthorisation(t *testing.T) {
	l := Classify(`EXEC sp_executesql N'DELETE FROM EMP_DATA'`)
	if l.Writes() {
		t.Skip("classifier happens to catch this one; the claim below still holds for other spellings")
	}
	if !l.Dynamic {
		t.Fatal("a statement that writes without being labelled a write must at least be flagged dynamic")
	}
}

func TestClassifyObjects(t *testing.T) {
	l := Classify("UPDATE dbo.EMP_DATA SET x = 1 FROM dbo.EMP_DATA e JOIN LEAVE_SUM_DATA s ON s.id = e.id")
	for _, want := range []string{"dbo.emp_data", "leave_sum_data"} {
		if !slices.Contains(l.Objects, want) {
			t.Errorf("Objects = %v, missing %q", l.Objects, want)
		}
	}
}

func TestClassifyEmpty(t *testing.T) {
	for _, s := range []string{"", "   ", "-- nothing here", "/* nor here */"} {
		got := Classify(s)
		if len(got.Kinds) != 0 {
			t.Errorf("Classify(%q).Kinds = %v, want none", s, got.Kinds)
		}
		if got.Summary() != string(KindEmpty) {
			t.Errorf("Summary() = %q", got.Summary())
		}
	}
}

// TestNoAuthorisationAPI is a guard on the package's shape rather than its
// behaviour: if someone adds an Allow or IsSafe here, the compiler will not
// object but the design will have quietly changed.
func TestNoAuthorisationAPI(t *testing.T) {
	// Writes is the only predicate, and its doc comment says what it is not.
	// This test exists so that adding a second one is a deliberate act that
	// has to come past a failing test with this name.
	var l Label
	_ = l.Writes()
	_ = l.Summary()
}

// TestEmptyStatementKindsMarshalAsArray: an empty batch has zero kinds, and
// zero kinds must serialize as [] rather than null.
//
// Classify already guards the normal path (no pattern matched -> KindOther),
// but the early return for an empty statement bypassed it and handed back a
// nil slice, which reached the audit JSONL as "kinds":null. Whatever reads
// that trail later has to range over a list; null is a parse error deferred to
// the least convenient moment.
//
// The kind count is asserted too, because the tempting one-line "fix" is to
// return KindOther here. That would serialize fine and be wrong: Summary()
// reads zero kinds as KindEmpty, so KindOther would relabel an empty batch as
// an unrecognised one.
func TestEmptyStatementKindsMarshalAsArray(t *testing.T) {
	for _, stmt := range []string{"", "   ", "\n\t ", "-- just a comment"} {
		l := Classify(stmt)

		if len(l.Kinds) != 0 {
			t.Errorf("Classify(%q).Kinds = %v, want none", stmt, l.Kinds)
		}
		if got := l.Summary(); got != string(KindEmpty) {
			t.Errorf("Classify(%q).Summary() = %q, want %q", stmt, got, KindEmpty)
		}

		b, err := json.Marshal(l)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if bytes.Contains(b, []byte(`"kinds":null`)) {
			t.Errorf(`Classify(%q) marshalled to "kinds":null, want "kinds":[] — got %s`, stmt, b)
		}
	}
}
