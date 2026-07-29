package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spaudit"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
)

// Every slice and map in an Out struct is omitempty, and that is load-bearing
// rather than cosmetic.
//
// A tool that refuses returns the zero Out alongside an IsError result. The
// SDK validates Out against the schema it inferred, and a field without
// omitempty is a required one — so a nil slice on the error path fails
// validation and the client receives a protocol error instead of the refusal
// the model was supposed to read. The failure only appears on error paths,
// which is where it is least likely to be noticed.

// ---------- mssql_targets ----------

type targetsIn struct{}

type targetsOut struct {
	Profile string                 `json:"profile"`
	Targets []service.TargetStatus `json:"targets,omitempty"`
}

func addTargets(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mssql_targets",
		Description: "List the databases this project may reach, and whether each currently passes the connection guard. Call this first: every other tool needs an alias from here.",
		Annotations: readOnly("Show reachable databases"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ targetsIn) (*mcp.CallToolResult, targetsOut, error) {
		out := targetsOut{Profile: svc.Policy().Profile, Targets: svc.Targets(ctx)}

		var b strings.Builder
		fmt.Fprintf(&b, "profile: %s\n", out.Profile)
		for _, t := range out.Targets {
			if t.Guard != "pass" {
				fmt.Fprintf(&b, "%-14s REJECTED BY GUARD — %s\n", t.Alias, t.Reason)
				continue
			}
			fmt.Fprintf(&b, "%-14s %s/%s  writable=%t  connect=%s\n",
				t.Alias, t.Server, t.Database, t.Writable, t.Connect)
		}
		return text(b.String()), out, nil
	})
}

// ---------- mssql_query ----------

type queryIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets. Optional only when the policy declares exactly one target."`
	SQL    string `json:"sql" jsonschema:"The T-SQL to run. Read-only: the login has DENY EXECUTE and no write permission, so writes are refused by SQL Server."`
	// MaxRows can only lower the policy cap. Saying so in the schema saves an
	// agent from trying to raise it and reading the result as a failure.
	MaxRows    int `json:"max_rows,omitempty" jsonschema:"Lower the row cap for this call. Cannot raise it above the policy limit."`
	TimeoutSec int `json:"timeout_sec,omitempty" jsonschema:"Lower the query timeout for this call. Cannot raise it above the policy limit."`
}

type queryOut struct {
	Target    targetEnvelope `json:"target"`
	Sets      []sqlrun.Set   `json:"sets,omitempty"`
	Rows      int            `json:"rows"`
	ElapsedMS int64          `json:"elapsed_ms"`
	Truncated bool           `json:"truncated"`
}

func addQuery(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_query",
		Description: "Run a read-only T-SQL statement. Results are capped by the project policy; " +
			"when the cap is hit the response says so rather than silently returning a prefix.",
		Annotations: readOnly("Run a read-only query"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
		res, t, err := svc.Query(ctx, in.Target, in.SQL, service.QueryOptions{
			MaxRows: in.MaxRows,
			Timeout: time.Duration(in.TimeoutSec) * time.Second,
		})
		if err != nil {
			return queryError(err), queryOut{}, nil
		}

		out := queryOut{
			Target:    envelope(svc.Describe(t)),
			Sets:      res.Sets,
			ElapsedMS: res.ElapsedMS,
			Truncated: res.Truncated,
		}
		for _, s := range res.Sets {
			out.Rows += len(s.Rows)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s — %d row(s) in %d ms\n", out.Target.header(), out.Rows, res.ElapsedMS)
		for i, set := range res.Sets {
			if len(res.Sets) > 1 {
				fmt.Fprintf(&b, "\n-- result set %d\n", i+1)
			}
			b.WriteString(renderSet(set))
			if set.Truncated {
				fmt.Fprintf(&b, "\n⚠ TRUNCATED by %s — this is not the whole answer. "+
					"Narrow the query rather than assuming these are all the rows.\n", set.TruncatedBy)
			}
		}
		return text(b.String()), out, nil
	})
}

// queryError separates the database's refusal from ours.
//
// An agent that cannot tell "you may not do that" from "that did not work"
// will retry, and retrying a permission denial is both useless and, in an
// audit log somebody reads later, indistinguishable from probing.
func queryError(err error) *mcp.CallToolResult {
	if n, ok := sqlrun.ServerErrorNumber(err); ok {
		switch n {
		case 229, 230, 262:
			return fail("SQL Server refused this on permissions (error %d): %v\n"+
				"This login is read-only by design. Do not retry; there is no variation of "+
				"this statement it is allowed to run.", n, err)
		case 1222:
			return fail("Lock request timed out (error 1222): %v\n"+
				"Another session holds a lock. The query was not run; retrying later may work.", err)
		}
		return fail("SQL Server error %d: %v", n, err)
	}
	return fail("%v", err)
}

func renderSet(set sqlrun.Set) string {
	if len(set.Columns) == 0 {
		return "(no columns)\n"
	}
	var b strings.Builder
	names := make([]string, len(set.Columns))
	for i, c := range set.Columns {
		names[i] = c.Name
	}
	b.WriteString(strings.Join(names, " | ") + "\n")
	for _, row := range set.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
				continue
			}
			cells[i] = strings.ReplaceAll(fmt.Sprint(v), "\n", " ")
		}
		b.WriteString(strings.Join(cells, " | ") + "\n")
	}
	return b.String()
}

// ---------- mssql_sp_list ----------

type spListIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	Filter string `json:"filter,omitempty" jsonschema:"Case-insensitive substring to match against procedure names."`
}

type spProc struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Created   string `json:"created"`
	Modified  string `json:"modified"`
	Encrypted bool   `json:"encrypted"`
}

type spListOut struct {
	Target targetEnvelope `json:"target"`
	Procs  []spProc       `json:"procedures,omitempty"`
	Total  int            `json:"total"`
}

func addSPList(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mssql_sp_list",
		Description: "List the stored procedures the database actually has, with their last-modified dates. This is the server's view, not the repository's — use mssql_sp_diff to compare the two.",
		Annotations: readOnly("List stored procedures"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in spListIn) (*mcp.CallToolResult, spListOut, error) {
		procs, t, err := svc.SPList(ctx, in.Target)
		if err != nil {
			return fail("%v", err), spListOut{}, nil
		}

		out := spListOut{Target: envelope(svc.Describe(t))}
		filter := strings.ToLower(in.Filter)
		for _, p := range procs {
			if filter != "" && !strings.Contains(strings.ToLower(p.Name), filter) {
				continue
			}
			out.Procs = append(out.Procs, spProc{
				Name:      p.Qualified(),
				Type:      p.Type,
				Created:   p.Created.Format("2006-01-02"),
				Modified:  p.Modified.Format("2006-01-02"),
				Encrypted: p.Encrypted,
			})
		}
		out.Total = len(procs)

		var b strings.Builder
		fmt.Fprintf(&b, "%s — %d of %d procedure(s)\n", out.Target.header(), len(out.Procs), out.Total)
		for _, p := range out.Procs {
			enc := ""
			if p.Encrypted {
				enc = "  [ENCRYPTED — definition not readable]"
			}
			fmt.Fprintf(&b, "%-40s modified %s%s\n", p.Name, p.Modified, enc)
		}
		return text(b.String()), out, nil
	})
}

// ---------- mssql_sp_get ----------

type spGetIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	Name   string `json:"name" jsonschema:"Procedure name, bare or schema-qualified (sp_x or dbo.sp_x)."`
}

type spGetOut struct {
	Target     targetEnvelope `json:"target"`
	Name       string         `json:"name"`
	Modified   string         `json:"modified"`
	Encrypted  bool           `json:"encrypted"`
	Definition string         `json:"definition,omitempty"`
}

func addSPGet(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mssql_sp_get",
		Description: "Read one stored procedure's definition as the server holds it (sys.sql_modules). This is what will actually run, which may differ from the script in the repository.",
		Annotations: readOnly("Read a stored procedure"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in spGetIn) (*mcp.CallToolResult, spGetOut, error) {
		p, t, err := svc.SPGet(ctx, in.Target, in.Name)
		if err != nil {
			return fail("%v", err), spGetOut{}, nil
		}
		out := spGetOut{
			Target:     envelope(svc.Describe(t)),
			Name:       p.Qualified(),
			Modified:   p.Modified.Format("2006-01-02 15:04"),
			Encrypted:  p.Encrypted,
			Definition: p.Definition,
		}
		if p.Encrypted {
			return text(fmt.Sprintf("%s — %s was created WITH ENCRYPTION; its definition cannot be read.",
				out.Target.header(), out.Name)), out, nil
		}
		return text(fmt.Sprintf("%s — %s, last modified %s\n\n%s",
			out.Target.header(), out.Name, out.Modified, p.Definition)), out, nil
	})
}

// ---------- mssql_sp_diff ----------

type spDiffIn struct {
	Target string   `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	Names  []string `json:"names,omitempty" jsonschema:"Procedure names to compare. Omit to compare every script in the repository."`
	// Defaulting to differences only keeps a whole-repository call readable;
	// the counts are always reported, so nothing is hidden by it.
	IncludeIdentical bool `json:"include_identical,omitempty" jsonschema:"Include procedures whose script and database definition match. Off by default."`
}

type spDiffOut struct {
	Target   targetEnvelope       `json:"target"`
	Results  []service.DiffResult `json:"results,omitempty"`
	Counts   map[string]int       `json:"counts,omitempty"`
	Failures map[string]string    `json:"failures,omitempty"`
}

func addSPDiff(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_sp_diff",
		Description: "Compare the stored-procedure scripts in the repository against the definitions in the database. " +
			"A 'differs' result means the script is not what runs.",
		Annotations: readOnly("Compare scripts against the database"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in spDiffIn) (*mcp.CallToolResult, spDiffOut, error) {
		res, t, err := svc.SPDiff(ctx, in.Target, in.Names, 3)
		if err != nil {
			return fail("%v", err), spDiffOut{}, nil
		}

		out := spDiffOut{
			Target:   envelope(svc.Describe(t)),
			Counts:   map[string]int{},
			Failures: res.Failures,
		}
		for _, r := range res.Results {
			out.Counts[string(r.Status)]++
			if r.Status == service.DiffIdentical && !in.IncludeIdentical {
				continue
			}
			out.Results = append(out.Results, r)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s\n", out.Target.header())
		fmt.Fprintf(&b, "identical=%d differs=%d file-only=%d db-only=%d unreadable=%d\n",
			out.Counts[string(service.DiffIdentical)], out.Counts[string(service.DiffDiffers)],
			out.Counts[string(service.DiffFileOnly)], out.Counts[string(service.DiffDBOnly)],
			out.Counts[string(service.DiffEncrypted)])
		for _, r := range out.Results {
			fmt.Fprintf(&b, "\n[%s] %s", r.Status, r.Name)
			if len(r.OtherFiles) > 0 {
				fmt.Fprintf(&b, "\n⚠ compared against %s; this procedure also has %d other script(s): %s",
					r.FilePath, len(r.OtherFiles), strings.Join(r.OtherFiles, ", "))
			}
			if r.Diff != "" {
				fmt.Fprintf(&b, "\n%s", r.Diff)
			}
		}
		if len(res.Failures) > 0 {
			fmt.Fprintf(&b, "\n%d file(s) could not be decoded and were NOT compared:\n", len(res.Failures))
			for f, e := range res.Failures {
				fmt.Fprintf(&b, "  %s: %s\n", f, e)
			}
		}
		return text(b.String()), out, nil
	})
}

// ---------- mssql_sp_audit ----------

type spAuditIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	Status string `json:"status,omitempty" jsonschema:"Show only rows with this status: ghost, differs, db-only, file-only, unreadable, orphan, identical."`
}

type auditRow struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	FilePath  string   `json:"file_path,omitempty"`
	DBName    string   `json:"db_name,omitempty"`
	CallSites []string `json:"call_sites,omitempty"`
}

type spAuditOut struct {
	Target       targetEnvelope `json:"target"`
	Counts       map[string]int `json:"counts,omitempty"`
	Rows         []auditRow     `json:"rows,omitempty"`
	Total        int            `json:"total"`
	HasFindings  bool           `json:"has_findings"`
	ScanFailures int            `json:"scan_failures"`
}

func addSPAudit(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_sp_audit",
		Description: "Three-way audit of stored procedures: repository scripts, database definitions, and Java call sites. " +
			"The status worth acting on is 'ghost' — Java calls it and the database does not have it.",
		Annotations: readOnly("Audit stored procedures three ways"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in spAuditIn) (*mcp.CallToolResult, spAuditOut, error) {
		// The report already carries its own target envelope, filled in with
		// the login that produced it, so the target value is not needed here.
		rep, _, err := svc.SPAudit(ctx, in.Target)
		if err != nil {
			return fail("%v", err), spAuditOut{}, nil
		}

		out := spAuditOut{
			Target:       envelope(rep.Target),
			Counts:       map[string]int{},
			Total:        len(rep.Rows),
			HasFindings:  rep.HasFindings(),
			ScanFailures: len(rep.FileFailures) + len(rep.JavaFailures),
		}
		for st, n := range rep.Counts {
			out.Counts[string(st)] = n
		}
		for _, r := range rep.Rows {
			if in.Status != "" && string(r.Status) != in.Status {
				continue
			}
			row := auditRow{Name: r.Name, Status: string(r.Status), FilePath: r.FilePath, DBName: r.DBName}
			for _, s := range r.Calls() {
				row.CallSites = append(row.CallSites, fmt.Sprintf("%s:%d", s.File, s.Line))
			}
			out.Rows = append(out.Rows, row)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s — %d procedure(s)\n", out.Target.header(), out.Total)
		for _, st := range spaudit.Order {
			fmt.Fprintf(&b, "  %-11s %3d  %s\n", st, rep.Counts[st], spaudit.Explain(st))
		}
		if out.ScanFailures > 0 {
			fmt.Fprintf(&b, "\n⚠ %d file(s) could not be decoded, so this audit is incomplete.\n", out.ScanFailures)
		}
		if in.Status != "" {
			fmt.Fprintf(&b, "\nrows with status %q:\n", in.Status)
			for _, r := range out.Rows {
				fmt.Fprintf(&b, "  %-32s %s\n", r.Name, strings.Join(r.CallSites, " "))
			}
		} else if ghosts := rep.ByStatus(spaudit.StatusGhost); len(ghosts) > 0 {
			b.WriteString("\nghosts (Java calls these, the database does not have them):\n")
			for _, r := range ghosts {
				sites := make([]string, 0, len(r.Calls()))
				for _, s := range r.Calls() {
					sites = append(sites, fmt.Sprintf("%s:%d", s.File, s.Line))
				}
				fmt.Fprintf(&b, "  %-32s %s\n", r.Name, strings.Join(sites, " "))
			}
		}
		return text(b.String()), out, nil
	})
}
