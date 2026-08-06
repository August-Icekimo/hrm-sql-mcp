package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/audit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/javascan"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spaudit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spfile"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// TargetStatus is one declared target and what the guard makes of it.
type TargetStatus struct {
	Alias    string `json:"alias"`
	Server   string `json:"server,omitempty"`
	Database string `json:"database,omitempty"`
	Writable bool   `json:"writable"`
	// Guard is "pass" or "rejected".
	Guard string `json:"guard"`
	// Connect is "ok" or the failure, so an unreachable target and a forbidden
	// one never look the same.
	Connect string `json:"connect"`
	Reason  string `json:"reason,omitempty"`
	// Note is the policy's description of what this target is.
	Note string `json:"note,omitempty"`
	// Snapshot dates the data. Two targets can differ only in when they were
	// taken, which is invisible from the alias alone.
	Snapshot *spdb.Snapshot `json:"snapshot,omitempty"`
	// CredentialFrom says where the read-only login was found — "env",
	// "file:<path>", or "MISSING". Never the credential itself.
	//
	// Worth a column because "no login configured" and "login configured but
	// the server refused it" produce different next actions, and without this
	// they arrive looking identical.
	CredentialFrom string `json:"credential_from,omitempty"`
}

// Targets reports every declared target with its guard and connection status.
func (s *Service) Targets(ctx context.Context) []TargetStatus {
	out := make([]TargetStatus, 0, len(s.pol.Targets))
	for _, alias := range s.Aliases() {
		pt, _ := s.pol.TargetByAlias(alias)
		st := TargetStatus{Alias: alias, Writable: pt.Writable, Guard: "rejected", Connect: "-", Note: pt.Note}
		if from, ok := s.creds.Origin(pt.CredentialKey, target.ReadOnly); ok {
			st.CredentialFrom = from
		} else {
			st.CredentialFrom = "MISSING"
		}

		t, err := s.reg.Check(ctx, alias, target.ReadOnly)
		if err != nil {
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		st.Guard, st.Server, st.Database = "pass", t.Addr(), t.Database()

		db, _, err := s.reg.Open(ctx, alias, target.ReadOnly)
		switch {
		case err != nil:
			st.Connect = "FAIL: " + err.Error()
		default:
			if perr := db.PingContext(ctx); perr != nil {
				st.Connect = "FAIL: " + perr.Error()
			} else {
				st.Connect = "ok"
				if snap, serr := spdb.SnapshotOf(ctx, db); serr == nil {
					st.Snapshot = &snap
				}
			}
		}
		out = append(out, st)
	}
	return out
}

// QueryOptions narrows the policy limits for one call.
type QueryOptions struct {
	// MaxRows and Timeout tighten the policy's values. They cannot loosen
	// them: a caller-supplied limit that could raise the cap would make the
	// policy advisory, and the policy is the part under code review.
	MaxRows int
	Timeout time.Duration
}

// Query runs a statement and returns bounded results.
func (s *Service) Query(ctx context.Context, alias, statement string, opt QueryOptions) (*sqlrun.Result, *target.Target, error) {
	const tool = "mssql_query"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}

	lim := s.queryLimits()
	if opt.MaxRows > 0 && opt.MaxRows < lim.MaxRows {
		lim.MaxRows = opt.MaxRows
	}
	if opt.Timeout > 0 && opt.Timeout < lim.Timeout {
		lim.Timeout = opt.Timeout
	}

	res, qerr := sqlrun.Query(ctx, db, statement, nil, lim)

	ev := audit.Event{Tool: tool, Statement: statement}
	if res != nil {
		ev.ElapsedMS = res.ElapsedMS
		ev.Truncated = res.Truncated
		for _, set := range res.Sets {
			ev.Rows += len(set.Rows)
		}
	}
	if err := s.finish(ev, t, s.login(ctx, db, t.Alias()), qerr); err != nil {
		return nil, t, err
	}
	return res, t, nil
}

// SPList returns the procedures the database has, without their definitions.
func (s *Service) SPList(ctx context.Context, alias string) ([]spdb.Proc, *target.Target, error) {
	const tool = "mssql_sp_list"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}
	cctx, cancel := s.catalogContext(ctx)
	defer cancel()

	procs, lerr := spdb.List(cctx, db)
	if err := s.finish(audit.Event{Tool: tool, Rows: len(procs)}, t, s.login(ctx, db, t.Alias()), lerr); err != nil {
		return nil, t, err
	}
	return procs, t, nil
}

// SPGet returns one procedure's definition from the database.
func (s *Service) SPGet(ctx context.Context, alias, name string) (spdb.Proc, *target.Target, error) {
	const tool = "mssql_sp_get"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return spdb.Proc{}, nil, err
	}
	cctx, cancel := s.catalogContext(ctx)
	defer cancel()

	proc, gerr := spdb.Get(cctx, db, name)
	ev := audit.Event{Tool: tool, Statement: name}
	if gerr == nil {
		ev.Rows = 1
	}
	if err := s.finish(ev, t, s.login(ctx, db, t.Alias()), gerr); err != nil {
		return spdb.Proc{}, t, err
	}
	return proc, t, nil
}

// DiffStatus classifies one procedure in a diff.
type DiffStatus string

const (
	DiffIdentical DiffStatus = "identical"
	DiffDiffers   DiffStatus = "differs"
	DiffFileOnly  DiffStatus = "file-only"
	DiffDBOnly    DiffStatus = "db-only"
	DiffEncrypted DiffStatus = "unreadable"
)

// DiffResult is one procedure compared between disk and database.
type DiffResult struct {
	Name     string     `json:"name"`
	Status   DiffStatus `json:"status"`
	FilePath string     `json:"file_path,omitempty"`
	DBName   string     `json:"db_name,omitempty"`
	Diff     string     `json:"diff,omitempty"`
	// OtherFiles are further scripts defining the same procedure. The diff is
	// against FilePath, which is whichever sorted first — deterministic but
	// arbitrary, so a caller reading the diff has to be told the choice was
	// made for them. Kept separate from Status so that "differs" keeps meaning
	// exactly one thing.
	OtherFiles []string `json:"other_files,omitempty"`
}

// SPDiffResult is the whole comparison.
type SPDiffResult struct {
	Results []DiffResult `json:"results"`
	// Failures are files that could not be decoded. Reported, never skipped:
	// a procedure missing from the comparison because its file was unreadable
	// is precisely the gap this tool exists to close.
	Failures map[string]string `json:"failures,omitempty"`
}

// SPDiff compares the scripts on disk against the database.
//
// Names may be empty to compare everything. Unlike SPAudit this does not scan
// the Java tree, which is most of the cost and answers a different question.
func (s *Service) SPDiff(ctx context.Context, alias string, names []string, diffContext int) (*SPDiffResult, *target.Target, error) {
	const tool = "mssql_sp_diff"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}

	spDir := s.ProjectPath(s.pol.Paths.SPDir)
	scripts, failures, serr := spfile.ScanDir(spDir)
	if serr != nil {
		return nil, t, s.finish(audit.Event{Tool: tool}, t, s.login(ctx, db, t.Alias()),
			fmt.Errorf("scan %s: %w", spDir, serr))
	}

	cctx, cancel := s.catalogContext(ctx)
	defer cancel()
	procs, lerr := spdb.LoadAll(cctx, db)
	if lerr != nil {
		return nil, t, s.finish(audit.Event{Tool: tool}, t, s.login(ctx, db, t.Alias()), lerr)
	}
	dbOf, _ := spdb.Index(procs)

	want := map[string]bool{}
	for _, n := range names {
		want[normaliseName(n)] = true
	}

	// Both fields are allocated up front: a nil map and a nil slice each
	// marshal to JSON null, and "results": null in `sp diff --format json`
	// breaks a reader that reasonably expects to range over a list.
	out := &SPDiffResult{
		Results:  make([]DiffResult, 0),
		Failures: map[string]string{},
	}
	for f, e := range failures {
		out.Failures[f] = e.Error()
	}

	// Group first, compare second. ScanDir returns files in name order, so the
	// first script for a procedure is a stable choice across runs and machines
	// — which matters because these results end up in a committed report.
	filesOf := map[string][]*spfile.Script{}
	var order []string
	for _, sc := range scripts {
		for _, name := range sc.Procs {
			if _, seen := filesOf[name]; !seen {
				order = append(order, name)
			}
			filesOf[name] = append(filesOf[name], sc)
		}
	}

	for _, name := range order {
		if len(want) > 0 && !want[name] {
			continue
		}
		files := filesOf[name]
		sc := files[0]
		r := DiffResult{Name: name, FilePath: s.relPath(sc.Path)}
		for _, other := range files[1:] {
			r.OtherFiles = append(r.OtherFiles, s.relPath(other.Path))
		}

		p, inDB := dbOf[name]
		switch {
		case !inDB:
			r.Status = DiffFileOnly
		case p.Encrypted:
			r.Status, r.DBName = DiffEncrypted, p.Qualified()
		default:
			def, _ := sc.DefinitionOf(name)
			r.DBName = p.Qualified()
			r.Diff = spfile.Diff(r.FilePath, def.Text, p.Qualified()+" (db)", p.Definition, diffContext)
			if r.Diff == "" {
				r.Status = DiffIdentical
			} else {
				r.Status = DiffDiffers
			}
		}
		out.Results = append(out.Results, r)
	}

	for _, p := range procs {
		name := p.Key()
		if len(want) > 0 && !want[name] {
			continue
		}
		if _, hasFile := filesOf[name]; !hasFile {
			out.Results = append(out.Results, DiffResult{
				Name: name, Status: DiffDBOnly, DBName: p.Qualified(),
			})
		}
	}

	if err := s.finish(audit.Event{Tool: tool, Rows: len(out.Results)}, t, s.login(ctx, db, t.Alias()), nil); err != nil {
		return nil, t, err
	}
	return out, t, nil
}

// SPAudit runs the three-way comparison: files, database, Java call sites.
func (s *Service) SPAudit(ctx context.Context, alias string) (*spaudit.Report, *target.Target, error) {
	const tool = "mssql_sp_audit"
	db, t, err := s.open(ctx, alias, tool)
	if err != nil {
		return nil, nil, err
	}
	login := s.login(ctx, db, t.Alias())

	src, serr := s.scanLocalSources()
	if serr != nil {
		return nil, t, s.finish(audit.Event{Tool: tool}, t, login, serr)
	}
	scripts, java, failures := src.scripts, src.java, src.failures

	cctx, cancel := s.catalogContext(ctx)
	defer cancel()
	procs, lerr := spdb.LoadAll(cctx, db)
	if lerr != nil {
		return nil, t, s.finish(audit.Event{Tool: tool}, t, login, lerr)
	}

	rep := spaudit.Build(spaudit.Inputs{
		Scripts:        scripts,
		Procs:          procs,
		Java:           java,
		ScriptFailures: failures,
		DiffContext:    3,
	})
	rep.Target = t.Describe(login)
	if snap, serr := spdb.SnapshotOf(cctx, db); serr == nil {
		rep.Snapshot = snap.String()
	}
	rep.SPDir = s.pol.Paths.SPDir
	rep.JavaDir = s.pol.Paths.JavaSrcDir

	if err := s.finish(audit.Event{Tool: tool, Rows: len(rep.Rows)}, t, login, nil); err != nil {
		return nil, t, err
	}
	return rep, t, nil
}

// SPAuditOffline compares only the two sources that need no server: the
// scripts in Stored Procedure/ and the Java call sites.
//
// This exists so the audit can gate a push from a machine with no container
// running and no credentials on it. It cannot answer whether anything is
// deployed or current, and the report it returns says so on its face rather
// than leaving a reader to infer it from a short findings list.
func (s *Service) SPAuditOffline(ctx context.Context) (*spaudit.Report, error) {
	const tool = "mssql_sp_audit_offline"
	// A distinct tool name, not a flag on the same one. Somebody reading the
	// audit log later needs to tell "compared against the database" from
	// "compared against nothing of the sort" without reconstructing the flags.
	src, serr := s.scanLocalSources()
	if serr != nil {
		return nil, s.finish(audit.Event{Tool: tool}, nil, "", serr)
	}

	rep := spaudit.Build(spaudit.Inputs{
		Scripts:        src.scripts,
		Java:           src.java,
		ScriptFailures: src.failures,
		DiffContext:    3,
		NoDB:           true,
	})
	rep.SPDir = s.pol.Paths.SPDir
	rep.JavaDir = s.pol.Paths.JavaSrcDir

	if err := s.finish(audit.Event{Tool: tool, Rows: len(rep.Rows)}, nil, "", nil); err != nil {
		return nil, err
	}
	return rep, nil
}

// localSources is what both audit lanes read off the filesystem.
type localSources struct {
	scripts  []*spfile.Script
	java     *javascan.Result
	failures map[string]error
}

// scanLocalSources reads the scripts and the Java tree. Shared so the offline
// lane cannot drift from the full one in what it scans or how it reports it —
// a divergence there would make the two reports quietly incomparable.
func (s *Service) scanLocalSources() (localSources, error) {
	var out localSources
	spDir := s.ProjectPath(s.pol.Paths.SPDir)
	javaDir := s.ProjectPath(s.pol.Paths.JavaSrcDir)

	scripts, failures, err := spfile.ScanDir(spDir)
	if err != nil {
		return out, fmt.Errorf("scan %s: %w", spDir, err)
	}
	// Every path in the report is project-relative. The absolute paths this
	// process happens to use would bake one developer's home directory into a
	// file that other people read.
	for _, sc := range scripts {
		sc.Path = filepath.Join(s.pol.Paths.SPDir, filepath.Base(sc.Path))
	}

	java, err := javascan.Scan(javaDir, javascan.Options{PathPrefix: s.pol.Paths.JavaSrcDir})
	if err != nil {
		return out, fmt.Errorf("scan %s: %w", javaDir, err)
	}
	out.scripts, out.java, out.failures = scripts, java, failures
	return out, nil
}

// relPath trims the project root so reports are portable between machines.
func (s *Service) relPath(p string) string {
	if rel, err := filepath.Rel(s.cfg.ProjectRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// normaliseName accepts dbo.sp_x, [dbo].[sp_x] and sp_x alike.
func normaliseName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.ReplaceAll(n, "[", "")
	n = strings.ReplaceAll(n, "]", "")
	if i := strings.LastIndex(n, "."); i >= 0 {
		n = n[i+1:]
	}
	return strings.ToLower(n)
}
