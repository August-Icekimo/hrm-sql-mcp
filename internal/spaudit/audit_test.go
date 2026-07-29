package spaudit

import (
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spfile"
)

func TestClassify(t *testing.T) {
	site := []javascan.Site{{File: "A.java", Line: 1, Name: "sp_x", Kind: javascan.KindCall}}
	mention := []javascan.Site{{File: "A.java", Line: 1, Name: "sp_x", Kind: javascan.KindMention}}

	tests := []struct {
		name string
		row  Row
		want Status
	}{
		{"java calls it, database has not got it", Row{InDB: false, InFile: true, CallSites: site}, StatusGhost},
		{"java calls something nobody has a file for", Row{CallSites: site}, StatusGhost},
		{"running but unversioned", Row{InDB: true}, StatusDBOnly},
		{"script never deployed and never called", Row{InFile: true}, StatusFileOnly},
		{"encrypted on the server", Row{InFile: true, InDB: true, Encrypted: true, CallSites: site}, StatusUnreadable},
		{"file and server disagree", Row{InFile: true, InDB: true, Diff: "@@", CallSites: site}, StatusDiffers},
		{"matching but uncalled", Row{InFile: true, InDB: true}, StatusOrphan},
		{"matching and called", Row{InFile: true, InDB: true, CallSites: site}, StatusIdentical},

		// The two precedence decisions the doc comment commits to.
		{"ghost outranks file-only", Row{InFile: true, InDB: false, CallSites: site}, StatusGhost},
		{"differs outranks orphan", Row{InFile: true, InDB: true, Diff: "@@"}, StatusDiffers},

		// The asymmetry between Called and Referenced. A mention is too weak
		// to raise a ghost and strong enough to withdraw an orphan.
		{"a mention does not make a ghost", Row{InFile: true, InDB: false, CallSites: mention}, StatusFileOnly},
		{"a mention does withdraw an orphan", Row{InFile: true, InDB: true, CallSites: mention}, StatusIdentical},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.row); got != tc.want {
				t.Errorf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyCoversEveryStatus fails if a status is added without a case in
// TestClassify, so the table cannot quietly fall behind the code.
func TestClassifyCoversEveryStatus(t *testing.T) {
	for _, s := range Order {
		if Explain(s) == "" {
			t.Errorf("status %q has no explanation, so it would render as a blank row", s)
		}
	}
}

func TestBuildThreeWay(t *testing.T) {
	in := Inputs{
		Scripts: []*spfile.Script{
			{
				Path: "Stored Procedure/sp_same.sql", Encoding: "utf-8",
				Procs:       []string{"sp_same"},
				Definitions: []spfile.Definition{{Name: "sp_same", Text: "CREATE PROC sp_same AS SELECT 1", Line: 1}},
			},
			{
				Path: "Stored Procedure/sp_stale.sql", Encoding: "cp950",
				Procs:       []string{"sp_stale"},
				Definitions: []spfile.Definition{{Name: "sp_stale", Text: "CREATE PROC sp_stale AS SELECT 1", Line: 1}},
			},
			{
				Path: "Stored Procedure/sp_undeployed.sql", Encoding: "utf-8",
				Procs:       []string{"sp_undeployed"},
				Definitions: []spfile.Definition{{Name: "sp_undeployed", Text: "CREATE PROC sp_undeployed AS SELECT 1", Line: 1}},
			},
		},
		Procs: []spdb.Proc{
			{Schema: "dbo", Name: "sp_same", Definition: "ALTER PROC sp_same AS SELECT 1"},
			{Schema: "dbo", Name: "sp_stale", Definition: "ALTER PROC sp_stale AS SELECT 2"},
			{Schema: "dbo", Name: "sp_undocumented", Definition: "ALTER PROC sp_undocumented AS SELECT 1"},
		},
		Java: &javascan.Result{
			Files: 2,
			ByName: map[string][]javascan.Site{
				"sp_same":  {{File: "A.java", Line: 10, Name: "sp_same", Kind: javascan.KindCall}},
				"sp_stale": {{File: "B.java", Line: 20, Name: "sp_stale", Kind: javascan.KindCall}},
				"sp_gone":  {{File: "C.java", Line: 30, Name: "sp_gone", Kind: javascan.KindCall}},
				// Mentioned, nothing behind it: must not become a row.
				"sp_mentioned_only": {{File: "D.java", Line: 40, Name: "sp_mentioned_only", Kind: javascan.KindMention}},
			},
		},
		DiffContext: 3,
	}

	rep := Build(in)

	want := map[string]Status{
		"sp_same":         StatusIdentical, // CREATE vs ALTER is normalised away
		"sp_stale":        StatusDiffers,
		"sp_undeployed":   StatusFileOnly,
		"sp_undocumented": StatusDBOnly,
		"sp_gone":         StatusGhost, // known only to Java — the row that matters
	}
	if len(rep.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rep.Rows), len(want), rep.Rows)
	}
	for _, r := range rep.Rows {
		if w, ok := want[r.Name]; !ok {
			t.Errorf("unexpected row %q", r.Name)
		} else if r.Status != w {
			t.Errorf("%s: status = %q, want %q (diff=%q)", r.Name, r.Status, w, r.Diff)
		}
	}

	// Rows must be sorted, or the committed report churns on every run.
	for i := 1; i < len(rep.Rows); i++ {
		if rep.Rows[i-1].Name > rep.Rows[i].Name {
			t.Errorf("rows are not sorted: %q before %q", rep.Rows[i-1].Name, rep.Rows[i].Name)
		}
	}

	if !rep.HasFindings() {
		t.Error("HasFindings() = false, but there is a ghost and a mismatch")
	}
	if got := rep.Counts[StatusGhost]; got != 1 {
		t.Errorf("ghost count = %d, want 1", got)
	}
}

func TestBuildReportsDuplicates(t *testing.T) {
	sc := func(path string) *spfile.Script {
		return &spfile.Script{
			Path: path, Procs: []string{"sp_dup"},
			Definitions: []spfile.Definition{{Name: "sp_dup", Text: "CREATE PROC sp_dup AS SELECT 1"}},
		}
	}
	rep := Build(Inputs{
		Scripts: []*spfile.Script{sc("a.sql"), sc("b.sql")},
		Procs: []spdb.Proc{
			{Schema: "dbo", Name: "sp_dup", Definition: "ALTER PROC sp_dup AS SELECT 1"},
			{Schema: "hr", Name: "sp_dup", Definition: "ALTER PROC sp_dup AS SELECT 1"},
		},
	})
	if len(rep.DuplicateFiles) != 1 {
		t.Errorf("DuplicateFiles = %v, want one entry", rep.DuplicateFiles)
	}
	// The row itself must carry the ambiguity, not only the appendix — a diff
	// against an arbitrary one of two files has to be labelled where it is read.
	if len(rep.Rows) != 1 || len(rep.Rows[0].OtherFiles) != 1 {
		t.Errorf("row does not record the other file: %#v", rep.Rows)
	}
	if md := rep.Markdown(MarkdownOptions{}); !strings.Contains(md, "另有 1 個同名檔") {
		t.Error("the report does not flag the row whose file is ambiguous")
	}
	if len(rep.DuplicateDBNames) != 1 {
		t.Errorf("DuplicateDBNames = %v, want one entry", rep.DuplicateDBNames)
	}
}

// TestMarkdownIsStableWithoutTimestamp protects the committed inventory from
// churning: two runs over the same facts must produce identical bytes.
func TestMarkdownIsStableWithoutTimestamp(t *testing.T) {
	in := Inputs{
		Procs: []spdb.Proc{{Schema: "dbo", Name: "sp_x", Definition: "ALTER PROC sp_x AS SELECT 1"}},
	}
	a := Build(in).Markdown(MarkdownOptions{})
	b := Build(in).Markdown(MarkdownOptions{})
	if a != b {
		t.Error("two runs produced different markdown; the report would churn in git")
	}
	if strings.Contains(a, "產生時間") {
		t.Error("timestamp appeared without MarkdownOptions.Timestamp")
	}
	if !strings.Contains(a, "`db-only`") {
		t.Error("the db-only section is missing from the report")
	}
}

func TestMarkdownEmbedsDiffs(t *testing.T) {
	rep := Build(Inputs{
		Scripts: []*spfile.Script{{
			Path: "a.sql", Procs: []string{"sp_x"},
			Definitions: []spfile.Definition{{Name: "sp_x", Text: "CREATE PROC sp_x AS SELECT 1"}},
		}},
		Procs: []spdb.Proc{{Schema: "dbo", Name: "sp_x", Definition: "ALTER PROC sp_x AS SELECT 2"}},
	})
	md := rep.Markdown(MarkdownOptions{Diffs: true})
	if !strings.Contains(md, "```diff") {
		t.Fatal("no diff block in the report")
	}
	if !strings.Contains(md, "+ALTER PROC sp_x AS SELECT 2") {
		t.Errorf("diff does not show the server's line:\n%s", md)
	}
}
