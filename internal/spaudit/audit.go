package spaudit

import (
	"sort"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spfile"
)

// Status is one row's classification.
type Status string

// The statuses, in the precedence Classify applies.
const (
	// StatusGhost: Java asks for it, the database does not have it. The only
	// status that describes a live defect rather than untidiness.
	StatusGhost Status = "ghost"
	// StatusDBOnly: running on the server with no script in the repository.
	// Nobody can review it and nobody can rebuild it.
	StatusDBOnly Status = "db-only"
	// StatusFileOnly: a script that was never deployed, and nothing calls it.
	StatusFileOnly Status = "file-only"
	// StatusUnreadable: created WITH ENCRYPTION, so no comparison is possible.
	StatusUnreadable Status = "unreadable"
	// StatusDiffers: both sides exist and disagree. The script is not what runs.
	StatusDiffers Status = "differs"
	// StatusOrphan: deployed and matching, but no Java call site found.
	StatusOrphan Status = "orphan"
	// StatusIdentical: deployed, matching, and called.
	StatusIdentical Status = "identical"
)

// Order is the order statuses appear in reports: most alarming first.
var Order = []Status{
	StatusGhost, StatusDiffers, StatusDBOnly, StatusFileOnly,
	StatusUnreadable, StatusOrphan, StatusIdentical,
}

// Explain gives the one-line meaning shown in the report's summary table.
func Explain(s Status) string {
	switch s {
	case StatusGhost:
		return "Java 會呼叫，但資料庫沒有 —— 等著發生的線上事故"
	case StatusDiffers:
		return "檔案與資料庫內容不同 —— 檔案不是實際在跑的版本"
	case StatusDBOnly:
		return "資料庫有，但沒有原始檔 —— 無法 review、無法重建"
	case StatusFileOnly:
		return "有原始檔，但資料庫沒有，也沒人呼叫 —— 未部署或已作廢"
	case StatusUnreadable:
		return "以 WITH ENCRYPTION 建立，定義不可讀 —— 無法比對"
	case StatusOrphan:
		return "檔案與資料庫一致，但 Java 找不到呼叫點 —— 待確認是否已死"
	case StatusIdentical:
		return "檔案、資料庫、Java 呼叫點三方一致"
	}
	return ""
}

// Row is one procedure across all three sources.
type Row struct {
	// Name is the lower-cased bare procedure name, the only identifier all
	// three sources share.
	Name   string
	Status Status

	InFile       bool
	FilePath     string
	FileEncoding string
	FileLine     int
	// OtherFiles are the further scripts defining the same procedure, which
	// HRM has 18 of — mostly dated variants like sp_SRB0400_1011210.sql kept
	// beside sp_SRB0400.sql. FilePath is whichever sorted first, so any diff
	// on such a row is against an arbitrary one of them and has to say so.
	OtherFiles []string

	InDB       bool
	DBName     string
	DBModified time.Time
	Encrypted  bool

	// CallSites are the Java literals naming this procedure, calls first.
	CallSites []javascan.Site

	// Diff is the unified diff, present only for StatusDiffers.
	Diff string
}

// Calls returns only the sites where the name appears in call position.
func (r Row) Calls() []javascan.Site {
	var out []javascan.Site
	for _, s := range r.CallSites {
		if s.Kind == javascan.KindCall {
			out = append(out, s)
		}
	}
	return out
}

// Called reports whether Java executes this procedure somewhere.
//
// Deliberately stricter than Referenced: this is what raises a ghost, and an
// alarm that fires on a column alias with a coincidental name is an alarm
// people learn to ignore.
func (r Row) Called() bool { return len(r.Calls()) > 0 }

// Referenced reports whether the name appears in Java at all, in any position.
//
// Deliberately looser than Called: this is what suppresses an orphan, and the
// cost of being wrong runs the other way. Declaring a procedure dead because
// its name is only assembled at runtime is how a live procedure gets deleted.
func (r Row) Referenced() bool { return len(r.CallSites) > 0 }

// Report is the full three-way comparison.
type Report struct {
	// Generated is when the audit ran.
	Generated time.Time
	// Target describes the database, so a report can never be mistaken for one
	// taken against a different server.
	Target map[string]string
	// SPDir and JavaDir are the scanned paths, as given.
	SPDir, JavaDir string

	Rows   []Row
	Counts map[Status]int

	// JavaFiles is how many source files were read.
	JavaFiles int
	// FileFailures and JavaFailures are files that could not be decoded.
	// A report with failures is a partial report and says so.
	FileFailures map[string]string
	JavaFailures map[string]string
	// DuplicateFiles lists procedures defined by more than one script, and
	// DuplicateDBNames those defined in more than one schema. Both make every
	// claim about that name ambiguous.
	DuplicateFiles   []string
	DuplicateDBNames []string
}

// Inputs are the three sources, already collected.
//
// Build takes gathered data rather than doing the gathering so the
// classification can be tested without a database, a source tree, or a
// filesystem — the part most likely to be wrong is the one hardest to reach
// through an integration test.
type Inputs struct {
	Scripts []*spfile.Script
	Procs   []spdb.Proc
	Java    *javascan.Result
	// ScriptFailures are files spfile.ScanDir could not decode.
	ScriptFailures map[string]error
	// DiffContext is the number of context lines in generated diffs.
	DiffContext int
}

// Build performs the three-way comparison.
func Build(in Inputs) *Report {
	rep := &Report{
		Generated:    time.Now(),
		Counts:       map[Status]int{},
		FileFailures: map[string]string{},
		JavaFailures: map[string]string{},
	}

	for f, err := range in.ScriptFailures {
		rep.FileFailures[f] = err.Error()
	}

	fileOf := map[string]*spfile.Script{}
	otherFiles := map[string][]string{}
	for _, sc := range in.Scripts {
		for _, name := range sc.Procs {
			if prev, dup := fileOf[name]; dup {
				rep.DuplicateFiles = append(rep.DuplicateFiles,
					name+": "+prev.Path+" / "+sc.Path)
				otherFiles[name] = append(otherFiles[name], sc.Path)
				continue
			}
			fileOf[name] = sc
		}
	}

	dbOf, dbDups := spdb.Index(in.Procs)
	rep.DuplicateDBNames = dbDups

	javaOf := map[string][]javascan.Site{}
	if in.Java != nil {
		javaOf = in.Java.ByName
		rep.JavaFiles = in.Java.Files
		for f, err := range in.Java.Failures {
			rep.JavaFailures[f] = err.Error()
		}
	}

	// The union of all three: a name known to any source deserves a row, and
	// the names known to only one are exactly the interesting ones.
	names := map[string]struct{}{}
	for n := range fileOf {
		names[n] = struct{}{}
	}
	for n := range dbOf {
		names[n] = struct{}{}
	}
	for n, sites := range javaOf {
		// A name Java only mentions, with no script and no procedure behind
		// it, is a string that follows the naming convention and nothing more.
		// Admitting it would put rows in the report that describe no object.
		if !hasCall(sites) {
			continue
		}
		names[n] = struct{}{}
	}

	for name := range names {
		row := buildRow(name, fileOf[name], dbOf, javaOf[name], in.DiffContext)
		row.OtherFiles = otherFiles[name]
		rep.Rows = append(rep.Rows, row)
	}

	sort.Slice(rep.Rows, func(i, j int) bool { return rep.Rows[i].Name < rep.Rows[j].Name })
	for _, r := range rep.Rows {
		rep.Counts[r.Status]++
	}
	return rep
}

func buildRow(name string, sc *spfile.Script, dbOf map[string]spdb.Proc, sites []javascan.Site, diffContext int) Row {
	r := Row{Name: name, CallSites: sites}

	var fileDef string
	if sc != nil {
		r.InFile = true
		r.FilePath = sc.Path
		r.FileEncoding = sc.Encoding
		if d, ok := sc.DefinitionOf(name); ok {
			fileDef = d.Text
			r.FileLine = d.Line
		}
	}

	proc, inDB := dbOf[name]
	if inDB {
		r.InDB = true
		r.DBName = proc.Qualified()
		r.DBModified = proc.Modified
		r.Encrypted = proc.Encrypted
	}

	if r.InFile && r.InDB && !r.Encrypted {
		r.Diff = spfile.Diff(r.FilePath, fileDef, r.DBName+" (db)", proc.Definition, diffContext)
	}
	r.Status = Classify(r)
	return r
}

// Classify assigns exactly one status, in this precedence:
//
//	ghost > db-only > file-only > unreadable > differs > orphan > identical
//
// The order encodes what a reader should act on first, not a taxonomy. Two
// choices in it are worth stating plainly:
//
//   - ghost outranks file-only. A script that exists but was never deployed,
//     while Java calls it, is still a production defect; filing it under
//     "file-only" would bury it among the harmless undeployed scripts.
//   - differs outranks orphan. If the script and the server disagree, that is
//     what needs fixing regardless of whether anything calls it — and the
//     "nothing calls it" claim rests on a literal scan that cannot see names
//     built at runtime, so it is the weaker signal of the two.
//
// Note the asymmetry between the two Java tests: ghost needs a call site,
// orphan needs the absence of any reference at all. Both settings err towards
// the quieter, less alarming answer, because a report that cries wolf and a
// report that green-lights a deletion are the two ways this becomes harmful.
func Classify(r Row) Status {
	switch {
	case !r.InDB && !r.InFile:
		// Known only to Java. Build admits these only when there is a call
		// site, so this is a ghost; the case is written out rather than left
		// to fall through to "identical", which is what it would otherwise do.
		return StatusGhost
	case !r.InDB && r.Called():
		return StatusGhost
	case r.InDB && !r.InFile:
		return StatusDBOnly
	case r.InFile && !r.InDB:
		return StatusFileOnly
	case r.Encrypted:
		return StatusUnreadable
	case r.Diff != "":
		return StatusDiffers
	case !r.Referenced():
		return StatusOrphan
	default:
		return StatusIdentical
	}
}

func hasCall(sites []javascan.Site) bool {
	for _, s := range sites {
		if s.Kind == javascan.KindCall {
			return true
		}
	}
	return false
}

// ByStatus returns the rows with the given status, keeping Rows' ordering.
func (rep *Report) ByStatus(s Status) []Row {
	var out []Row
	for _, r := range rep.Rows {
		if r.Status == s {
			out = append(out, r)
		}
	}
	return out
}

// HasFindings reports whether anything needs a human, which is what a CI step
// would gate on.
func (rep *Report) HasFindings() bool {
	return rep.Counts[StatusGhost] > 0 ||
		rep.Counts[StatusDiffers] > 0 ||
		rep.Counts[StatusDBOnly] > 0 ||
		len(rep.FileFailures) > 0 ||
		len(rep.JavaFailures) > 0
}
