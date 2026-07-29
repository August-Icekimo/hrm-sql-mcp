package spaudit

import (
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spfile"
)

func TestClassifyOffline(t *testing.T) {
	site := []javascan.Site{{File: "A.java", Line: 1, Name: "sp_x", Kind: javascan.KindCall}}
	mention := []javascan.Site{{File: "A.java", Line: 1, Name: "sp_x", Kind: javascan.KindMention}}

	tests := []struct {
		name string
		row  Row
		want Status
	}{
		{"java calls it and no script exists", Row{DBUnknown: true, CallSites: site}, StatusMissingScript},
		{"script nothing mentions", Row{DBUnknown: true, InFile: true}, StatusUnreferenced},
		{"script java calls", Row{DBUnknown: true, InFile: true, CallSites: site}, StatusScripted},

		// The same asymmetry the online path uses, for the same reasons: a
		// coincidental column alias must not raise missing-script, and any
		// mention at all must withdraw unreferenced.
		{"a mention does not make a missing-script", Row{DBUnknown: true, CallSites: mention}, StatusScripted},
		{"a mention does withdraw unreferenced", Row{DBUnknown: true, InFile: true, CallSites: mention}, StatusScripted},

		// InDB is meaningless when the database was never asked. Whatever it
		// holds must not reach the verdict.
		{"stray InDB is ignored offline", Row{DBUnknown: true, InDB: true, InFile: true, CallSites: site}, StatusScripted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.row); got != tc.want {
				t.Errorf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOfflineStatusesAllExplained mirrors its online twin: a status added to
// OfflineOrder without an explanation would render as a blank table row.
func TestOfflineStatusesAllExplained(t *testing.T) {
	for _, s := range OfflineOrder {
		if Explain(s) == "" {
			t.Errorf("status %q has no explanation, so it would render as a blank row", s)
		}
	}
}

// TestBuildOfflineInventsNothing is the regression this whole lane exists to
// avoid. Given the same sources as the three-way test but no database, every
// row must carry an offline status — never file-only, db-only, ghost or
// identical, each of which asserts something about a server nobody asked.
func TestBuildOfflineInventsNothing(t *testing.T) {
	rep := Build(offlineInputs())

	want := map[string]Status{
		"sp_same":       StatusScripted,      // java calls it; deployment unknown
		"sp_stale":      StatusScripted,      // the drift is invisible without the server
		"sp_undeployed": StatusUnreferenced,  // a script nothing mentions
		"sp_gone":       StatusMissingScript, // java calls it, no script
	}
	if len(rep.Rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rep.Rows), len(want), rep.Rows)
	}
	for _, r := range rep.Rows {
		w, ok := want[r.Name]
		if !ok {
			t.Errorf("unexpected row %q", r.Name)
			continue
		}
		if r.Status != w {
			t.Errorf("%s: status = %q, want %q", r.Name, r.Status, w)
		}
		if !r.DBUnknown {
			t.Errorf("%s: DBUnknown = false in an offline report", r.Name)
		}
		if r.Diff != "" {
			t.Errorf("%s: has a diff, but there was nothing to diff against", r.Name)
		}
	}

	// sp_undocumented existed only in the database in the three-way test. With
	// no database it must not appear at all — a report that omits it is honest,
	// one that lists it as anything is fabricating.
	for _, r := range rep.Rows {
		if r.Name == "sp_undocumented" {
			t.Error("a database-only procedure appeared in a report built without a database")
		}
	}

	if !rep.Offline {
		t.Error("Offline = false on a report built with NoDB")
	}
}

// TestOfflineGateIgnoresHousekeeping pins what the pre-push hook fires on.
// unreferenced is untidiness; a gate that fires on it is one people disable.
func TestOfflineGateIgnoresHousekeeping(t *testing.T) {
	in := offlineInputs()
	in.Java = &javascan.Result{Files: 1, ByName: map[string][]javascan.Site{}}
	rep := Build(in)

	if rep.Counts[StatusUnreferenced] == 0 {
		t.Fatal("test setup produced no unreferenced rows")
	}
	if rep.Counts[StatusMissingScript] != 0 {
		t.Fatalf("test setup produced %d missing-script rows, want 0", rep.Counts[StatusMissingScript])
	}
	if rep.HasFindings() {
		t.Error("HasFindings() = true with only unreferenced scripts; the gate would fire on housekeeping")
	}

	// And it must fire on the one status that does assert a defect.
	if !Build(offlineInputs()).HasFindings() {
		t.Error("HasFindings() = false despite a missing-script row")
	}
}

// TestOfflineGateFiresOnUndecodableFiles keeps a partial scan from reading as
// an all-clear: if a script could not be decoded, the audit did not see it.
func TestOfflineGateFiresOnUndecodableFiles(t *testing.T) {
	in := offlineInputs()
	in.Java = &javascan.Result{Files: 1, ByName: map[string][]javascan.Site{}}
	in.ScriptFailures = map[string]error{"Stored Procedure/sp_mojibake.sql": errDecode{}}

	if !Build(in).HasFindings() {
		t.Error("HasFindings() = false with an undecodable script")
	}
}

type errDecode struct{}

func (errDecode) Error() string { return "could not determine encoding" }

// TestMarkdownOfflineDeclaresItself is the difference between a short findings
// list meaning "clean" and meaning "nobody looked".
func TestMarkdownOfflineDeclaresItself(t *testing.T) {
	md := Build(offlineInputs()).Markdown(MarkdownOptions{})

	for _, want := range []string{"離線盤點", "資料庫沒有被查詢過", "missing-script"} {
		if !strings.Contains(md, want) {
			t.Errorf("offline report does not contain %q", want)
		}
	}
	// The online-only statuses head sections that cannot be filled in offline.
	for _, s := range []Status{StatusGhost, StatusDBOnly, StatusFileOnly, StatusIdentical} {
		if strings.Contains(md, "## `"+string(s)+"`") {
			t.Errorf("offline report has a %q section, which needs a database", s)
		}
	}
}

// TestMarkdownOfflineIsReproducible is what lets --check gate a push: the
// report must be a pure function of the repository, or it fails on whichever
// machine regenerates it second.
func TestMarkdownOfflineIsReproducible(t *testing.T) {
	a := Build(offlineInputs()).Markdown(MarkdownOptions{})
	b := Build(offlineInputs()).Markdown(MarkdownOptions{})
	if a != b {
		t.Error("two runs over identical inputs produced different documents")
	}
	if strings.Contains(a, "產生時間") {
		t.Error("the default report carries a timestamp, so every run would show as a change")
	}
}

// offlineInputs mirrors TestBuildThreeWay's sources, minus the database. The
// Procs entry is deliberately present: NoDB must win over it, since a caller
// passing both is the mistake most likely to be made.
func offlineInputs() Inputs {
	return Inputs{
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
			{Schema: "dbo", Name: "sp_undocumented", Definition: "ALTER PROC sp_undocumented AS SELECT 1"},
		},
		Java: &javascan.Result{
			Files: 2,
			ByName: map[string][]javascan.Site{
				"sp_same":  {{File: "A.java", Line: 10, Name: "sp_same", Kind: javascan.KindCall}},
				"sp_stale": {{File: "B.java", Line: 20, Name: "sp_stale", Kind: javascan.KindCall}},
				"sp_gone":  {{File: "C.java", Line: 30, Name: "sp_gone", Kind: javascan.KindCall}},
			},
		},
		DiffContext: 3,
		NoDB:        true,
	}
}
