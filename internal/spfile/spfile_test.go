package spfile

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func utf16le(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

func TestDecodeReportsEncoding(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
		text string
	}{
		{"utf16le bom", utf16le("CREATE PROC 測試"), EncUTF16LE, "CREATE PROC 測試"},
		{"utf8 bom", append([]byte{0xEF, 0xBB, 0xBF}, "SELECT 1"...), EncUTF8BOM, "SELECT 1"},
		{"plain utf8", []byte("SELECT 員工"), EncUTF8, "SELECT 員工"},
		{"ascii", []byte("SELECT 1"), EncUTF8, "SELECT 1"},
		// 0xA4 0x40 is 一 in Big5; it is not valid UTF-8, so it must fall through.
		{"cp950", []byte{'-', '-', ' ', 0xA4, 0x40}, EncCP950, "-- 一"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, enc, err := Decode(c.raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if enc != c.want {
				t.Errorf("encoding = %q, want %q", enc, c.want)
			}
			if got != c.text {
				t.Errorf("text = %q, want %q", got, c.text)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8")
			}
		})
	}
}

// TestStripLiterals pins the cases that make naive scanning wrong. Each of
// these would otherwise cause a false match for a batch separator or a
// procedure header.
func TestStripLiterals(t *testing.T) {
	cases := []struct{ in, keep string }{
		{"SELECT 'GO'", "SELECT"},
		{"SELECT 'it''s GO'", "SELECT"},
		{"-- GO in a comment", ""},
		{"/* GO */ SELECT 1", "SELECT 1"},
		{"/* outer /* inner */ still */ SELECT 2", "SELECT 2"},
		{"SELECT [GO], \"GO\"", "SELECT"},
	}
	for _, c := range cases {
		got := StripLiterals(c.in)
		if len(got) != len(c.in) {
			t.Errorf("StripLiterals(%q) changed length %d -> %d (offsets must stay aligned)",
				c.in, len(c.in), len(got))
		}
		if strings.Contains(got, "GO") {
			t.Errorf("StripLiterals(%q) = %q, still contains GO", c.in, got)
		}
		if c.keep != "" && !strings.Contains(got, c.keep) {
			t.Errorf("StripLiterals(%q) = %q, lost %q", c.in, got, c.keep)
		}
	}
}

func TestSplitBatches(t *testing.T) {
	script := "SELECT 1\nGO\nSELECT 2\ngo 5\nSELECT 3\n"
	b := SplitBatches(script)
	if len(b) != 3 {
		t.Fatalf("got %d batches, want 3: %#v", len(b), b)
	}
	if !strings.Contains(b[1].Text, "SELECT 2") {
		t.Errorf("batch 1 = %q", b[1].Text)
	}

	// A GO that is not alone on its line is not a separator.
	if got := SplitBatches("SELECT 1\nGO SELECT 2\n"); len(got) != 1 {
		t.Errorf("`GO SELECT 2` must not split; got %d batches", len(got))
	}
	// A GO inside a comment is not a separator.
	if got := SplitBatches("SELECT 1\n-- GO\nSELECT 2\n"); len(got) != 1 {
		t.Errorf("commented GO must not split; got %d batches", len(got))
	}
}

func TestParseTextFindsProcs(t *testing.T) {
	script := `
-- this mentions CREATE PROC sp_not_real in a comment
GO
CREATE PROCEDURE [dbo].[sp_Real_One]
AS
BEGIN
    SELECT 1
END
GO
ALTER PROC sp_second AS SELECT 2
`
	s := ParseText(script)
	want := []string{"sp_real_one", "sp_second"}
	if len(s.Procs) != len(want) {
		t.Fatalf("procs = %v, want %v", s.Procs, want)
	}
	for i := range want {
		if s.Procs[i] != want[i] {
			t.Errorf("procs[%d] = %q, want %q", i, s.Procs[i], want[i])
		}
	}
	if !strings.Contains(s.Definition, "sp_Real_One") {
		t.Errorf("definition should be the first proc batch, got %q", s.Definition)
	}
}

// TestNormalizeIsRestrained is the important one. A normaliser that is too
// aggressive reports "identical" for procedures that genuinely differ, which
// is worse than having no tool.
func TestNormalizeIsRestrained(t *testing.T) {
	base := "CREATE PROC sp_x\nAS\n    SELECT 1\n"

	shouldMatch := []struct {
		name, other string
	}{
		{"CRLF", "CREATE PROC sp_x\r\nAS\r\n    SELECT 1\r\n"},
		{"trailing spaces", "CREATE PROC sp_x   \nAS\t\n    SELECT 1  \n"},
		{"trailing blank lines", base + "\n\n\n"},
		{"CREATE vs ALTER", "ALTER PROC sp_x\nAS\n    SELECT 1\n"},
	}
	for _, c := range shouldMatch {
		if !Equal(base, c.other) {
			t.Errorf("%s: should compare equal\n  a=%q\n  b=%q", c.name, Normalize(base), Normalize(c.other))
		}
	}

	shouldDiffer := []struct {
		name, other string
	}{
		{"different indentation", "CREATE PROC sp_x\nAS\n        SELECT 1\n"},
		{"different case", "CREATE PROC sp_x\nAS\n    select 1\n"},
		{"extra inner spaces", "CREATE PROC sp_x\nAS\n    SELECT  1\n"},
		{"different body", "CREATE PROC sp_x\nAS\n    SELECT 2\n"},
	}
	for _, c := range shouldDiffer {
		if Equal(base, c.other) {
			t.Errorf("%s: must NOT compare equal — smoothing this over makes the tool a placebo", c.name)
		}
	}
}

func TestDiff(t *testing.T) {
	a := "CREATE PROC sp_x\nAS\nSELECT 1\nSELECT 2\n"
	b := "ALTER PROC sp_x\nAS\nSELECT 1\n-- added\nSELECT 2\n"

	if d := Diff("file", a, "server", a, 3); d != "" {
		t.Errorf("identical input must produce empty diff, got:\n%s", d)
	}
	d := Diff("file", a, "server", b, 3)
	if d == "" {
		t.Fatal("expected a diff")
	}
	if !strings.Contains(d, "+-- added") {
		t.Errorf("diff should show the added line:\n%s", d)
	}
	// CREATE vs ALTER is normalised away, so it must not appear as a change.
	if strings.Contains(d, "-CREATE") || strings.Contains(d, "+ALTER") {
		t.Errorf("CREATE/ALTER must be normalised, not reported:\n%s", d)
	}
	// Count body lines only; the "+++ server" / "--- file" headers also begin
	// with + and -, and counting those was this test's own first bug.
	var adds, dels int
	for _, ln := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"):
		case strings.HasPrefix(ln, "+"):
			adds++
		case strings.HasPrefix(ln, "-"):
			dels++
		}
	}
	if adds != 1 || dels != 0 {
		t.Errorf("expected exactly 1 addition and 0 deletions, got +%d -%d:\n%s", adds, dels, d)
	}
}
