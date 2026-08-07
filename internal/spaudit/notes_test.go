package spaudit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
)

func TestLoadNotes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("missing file is not an error", func(t *testing.T) {
		// A project that has never written notes must not see failures, and an
		// empty path (the field unset in the policy) means the same thing.
		for _, p := range []string{filepath.Join(dir, "nope.yaml"), "", "   "} {
			n, err := LoadNotes(p)
			if err != nil {
				t.Errorf("LoadNotes(%q) = %v, want no error", p, err)
			}
			if n != nil {
				t.Errorf("LoadNotes(%q) = %v, want nil", p, n)
			}
		}
	})

	t.Run("malformed file is an error", func(t *testing.T) {
		// Never silently: a typo here would otherwise cost every note in the
		// file, which is exactly the loss this mechanism exists to prevent.
		p := write("bad.yaml", "sp_x: [this is a list, not a string]\n")
		if _, err := LoadNotes(p); err == nil {
			t.Error("LoadNotes accepted a file whose values are not strings")
		}
	})

	t.Run("names match case-insensitively", func(t *testing.T) {
		// The report lower-cases names; scripts and people do not.
		p := write("ok.yaml", "sp_WDC0100: |\n  已退役\n")
		n, err := LoadNotes(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, probe := range []string{"sp_WDC0100", "sp_wdc0100", "SP_WDC0100", " sp_wdc0100 "} {
			if got := n.Get(probe); got != "已退役" {
				t.Errorf("Get(%q) = %q, want 已退役", probe, got)
			}
		}
	})

	t.Run("blank entries are dropped", func(t *testing.T) {
		p := write("blank.yaml", "sp_a: \"\"\nsp_b: \"   \"\nsp_c: real\n")
		n, err := LoadNotes(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := n.Names(); len(got) != 1 || got[0] != "sp_c" {
			t.Errorf("Names() = %v, want [sp_c]", got)
		}
	})
}

// TestNotesAnnotateButNeverSuppress is the invariant that makes this mechanism
// safe to have at all.
//
// A note must not be able to change a row's status, remove it from a section,
// or alter a count. The moment it can, it becomes a way to silence findings —
// and an inventory whose clean sections cannot be trusted is worth less than
// no inventory. The report is allowed to say "somebody looked into this"; it
// is not allowed to say "so it does not count".
//
// The procedure annotated here is deliberately one the audit calls healthy
// (`identical`), because that is the real case: sp_WDC0100 has a live-looking
// Java call site and is nonetheless retired. The note must appear without the
// row leaving `identical`.
func TestNotesAnnotateButNeverSuppress(t *testing.T) {
	call := []javascan.Site{{File: "WDC0100Action.java", Line: 672, Name: "sp_wdc0100", Kind: javascan.KindCall}}

	build := func(notes Notes) *Report {
		rep := &Report{
			Rows: []Row{
				{Name: "sp_wdc0100", InFile: true, InDB: true, CallSites: call, Status: StatusIdentical},
				{Name: "sp_other", InFile: true, InDB: true, CallSites: call, Status: StatusIdentical},
			},
			Counts: map[Status]int{StatusIdentical: 2},
			Notes:  notes,
		}
		return rep
	}

	plain := build(nil).Markdown(MarkdownOptions{})
	noted := build(Notes{"sp_wdc0100": "已退役：MENU 已下線，無人有執行權限。"}).Markdown(MarkdownOptions{})

	if strings.Contains(plain, "人工註記") {
		t.Error("a report with no notes rendered the notes section")
	}
	if !strings.Contains(noted, "人工註記") {
		t.Fatal("the notes section is missing")
	}
	if !strings.Contains(noted, "已退役：MENU 已下線，無人有執行權限。") {
		t.Error("the note text is missing")
	}

	// The status is reported alongside the note, not replaced by it.
	if !strings.Contains(noted, "本次盤點狀態：`identical`") {
		t.Error("the note does not carry the status the audit computed")
	}

	// Everything the plain report said, the annotated one still says. Compared
	// line by line so an accidental drop anywhere is caught, not just in the
	// counts.
	for _, line := range strings.Split(plain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(noted, line) {
			t.Errorf("annotating removed a line from the report: %q", line)
		}
	}
}

// TestNoteForAbsentProcedureSaysSo covers the note that outlives its subject:
// somebody records a finding, the procedure is later dropped, and the note is
// still in the file. Reporting it as though it described something present
// would be a small lie that compounds.
func TestNoteForAbsentProcedureSaysSo(t *testing.T) {
	rep := &Report{
		Rows:   []Row{{Name: "sp_present", InFile: true, InDB: true, Status: StatusOrphan}},
		Counts: map[Status]int{StatusOrphan: 1},
		Notes:  Notes{"sp_long_gone": "2026-08 查證後已退役"},
	}
	md := rep.Markdown(MarkdownOptions{})
	if !strings.Contains(md, "（本次盤點未出現此程序）") {
		t.Errorf("a note for a procedure not in the report did not say so:\n%s", md)
	}
}
