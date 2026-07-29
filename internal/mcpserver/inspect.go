package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/schemadict"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
)

// ---------- mssql_explain ----------

type explainIn struct {
	Target     string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	SQL        string `json:"sql" jsonschema:"The statement to plan. It is compiled, not executed."`
	IncludeXML bool   `json:"include_xml,omitempty" jsonschema:"Return the raw showplan XML as well. It is large; the summary is usually enough."`
}

type explainOut struct {
	Target         targetEnvelope         `json:"target"`
	Statements     []sqlrun.PlanStatement `json:"statements,omitempty"`
	TotalCost      float64                `json:"total_cost"`
	Scans          []sqlrun.PlanOperator  `json:"scans,omitempty"`
	TopOperators   []sqlrun.PlanOperator  `json:"top_operators,omitempty"`
	MissingIndexes []sqlrun.MissingIndex  `json:"missing_indexes,omitempty"`
	Warnings       []sqlrun.PlanWarning   `json:"warnings,omitempty"`
	XML            string                 `json:"xml,omitempty"`
	XMLBytes       int                    `json:"xml_bytes"`
}

func addExplain(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_explain",
		Description: "Get the estimated execution plan for a statement without running it. " +
			"Use this before running anything expensive, and to find out why a query is slow: " +
			"it reports estimated cost, scan operators, implicit-conversion warnings and the " +
			"server's own missing-index suggestions.",
		Annotations: readOnly("Explain a query without running it"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in explainIn) (*mcp.CallToolResult, explainOut, error) {
		plan, t, err := svc.Explain(ctx, in.Target, in.SQL, in.IncludeXML)
		if err != nil {
			return queryError(err), explainOut{}, nil
		}

		const topN = 5
		top := plan.Operators
		if len(top) > topN {
			top = top[:topN]
		}
		out := explainOut{
			Target:         envelope(svc.Describe(t)),
			Statements:     plan.Statements,
			TotalCost:      plan.TotalCost(),
			Scans:          plan.Scans(),
			TopOperators:   top,
			MissingIndexes: plan.MissingIndexes,
			Warnings:       plan.Warnings,
			XML:            plan.XML,
			XMLBytes:       plan.XMLBytes,
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s — estimated only, nothing was executed\n", out.Target.header())
		for _, s := range plan.Statements {
			fmt.Fprintf(&b, "\n%s\n  estimated rows %.0f, cost %.4f\n", oneLine(s.Text), s.EstimatedRows, s.EstimatedCost)
		}
		if scans := plan.Scans(); len(scans) > 0 {
			b.WriteString("\nscans (these grow with the table):\n")
			for _, o := range scans {
				fmt.Fprintf(&b, "  %-28s est rows %.0f, subtree cost %.4f\n", o.Physical, o.EstimatedRows, o.EstimatedCost)
			}
		}
		if len(plan.Warnings) > 0 {
			b.WriteString("\nwarnings:\n")
			for _, w := range plan.Warnings {
				fmt.Fprintf(&b, "  %s %s\n", w.Kind, w.Detail)
			}
		}
		if len(plan.MissingIndexes) > 0 {
			b.WriteString("\nthe optimiser suggests indexes (a suggestion, not a verdict — " +
				"adding one costs write throughput and someone has to own that trade):\n")
			for _, mi := range plan.MissingIndexes {
				fmt.Fprintf(&b, "  %s impact %.1f%%  equality=%v inequality=%v include=%v\n",
					mi.Table, mi.Impact, mi.Equality, mi.Inequality, mi.Include)
			}
		}
		fmt.Fprintf(&b, "\ntotal estimated cost %.4f (plan XML %d bytes)\n", out.TotalCost, plan.XMLBytes)
		return text(b.String()), out, nil
	})
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ---------- mssql_deps ----------

type depsIn struct {
	Target    string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets."`
	Name      string `json:"name" jsonschema:"Object name, bare or schema-qualified (sp_x or dbo.sp_x)."`
	Direction string `json:"direction,omitempty" jsonschema:"uses (what this object references), used_by (what references it), or both. Default both."`
}

type depsOut struct {
	Target       targetEnvelope    `json:"target"`
	Name         string            `json:"name"`
	Dependencies []spdb.Dependency `json:"dependencies,omitempty"`
	// Caveat travels with the data rather than only in the tool description,
	// because an empty dependency list is the result most likely to be
	// mistaken for proof that nothing uses an object.
	Caveat string `json:"caveat"`
}

const depsCaveat = "SQL-to-SQL references only. Names built at run time (EXEC(@sql)) are invisible " +
	"to the catalog, and application call sites are not here at all — use mssql_sp_audit for those. " +
	"An empty result means 'no other SQL module references this', not 'this is unused'."

func addDeps(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_deps",
		Description: "List what a stored procedure, view or table references, and what references it, " +
			"from sys.sql_expression_dependencies. " + depsCaveat,
		Annotations: readOnly("Show object dependencies"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in depsIn) (*mcp.CallToolResult, depsOut, error) {
		dir := spdb.Direction(in.Direction)
		if in.Direction == "" {
			dir = spdb.Both
		}

		deps, t, err := svc.Deps(ctx, in.Target, in.Name, dir)
		if err != nil {
			return fail("%v", err), depsOut{}, nil
		}

		out := depsOut{
			Target:       envelope(svc.Describe(t)),
			Name:         in.Name,
			Dependencies: deps,
			Caveat:       depsCaveat,
		}

		var b strings.Builder
		fmt.Fprintf(&b, "%s — %s\n", out.Target.header(), in.Name)
		writeDeps(&b, "references", deps, spdb.Uses)
		writeDeps(&b, "referenced by", deps, spdb.UsedBy)
		fmt.Fprintf(&b, "\nnote: %s\n", depsCaveat)
		return text(b.String()), out, nil
	})
}

func writeDeps(b *strings.Builder, label string, deps []spdb.Dependency, dir spdb.Direction) {
	var rows []spdb.Dependency
	for _, d := range deps {
		if d.Direction == dir {
			rows = append(rows, d)
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s (%d):\n", label, len(rows))
	for _, d := range rows {
		flags := ""
		if !d.Exists {
			flags += "  ⚠ MISSING — referenced but not present in this database"
		}
		if d.CallerDependent {
			flags += "  [resolved at run time]"
		}
		if d.Ambiguous {
			flags += "  [ambiguous]"
		}
		fmt.Fprintf(b, "  %-44s %s%s\n", d.Name, d.Type, flags)
	}
}

// ---------- mssql_schema_search ----------

type schemaSearchIn struct {
	Query string `json:"query" jsonschema:"Substring to look for. Matches table names, column names and the Chinese descriptions alike, so either 特休 or leave_days will find the column."`
	// The tables-only mode exists because "which tables are about X" and
	// "which column holds X" are different questions, and column hits drown
	// out table hits when the answer is a table.
	TablesOnly bool `json:"tables_only,omitempty" jsonschema:"Match only table names and business-area names, not columns."`
	Limit      int  `json:"limit,omitempty" jsonschema:"Maximum matches to return. Default 50."`
}

type schemaSearchOut struct {
	// Source is the dictionary file the answers came from. Returned so a
	// caller can tell a documented claim from a live one, and check it.
	Source  string             `json:"source"`
	Query   string             `json:"query"`
	Matches []schemadict.Match `json:"matches,omitempty"`
	Note    string             `json:"note"`
}

const dictNote = "From the repository's data dictionary, not the live database. It can be stale, " +
	"and its 主鍵 column is unreliable. Confirm anything load-bearing with mssql_query against INFORMATION_SCHEMA."

func addSchemaSearch(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_schema_search",
		Description: "Search the project's data dictionary for tables and columns, by identifier or by " +
			"Chinese description. Use this to turn a business term into a column name before writing a query. " +
			dictNote,
		Annotations: readOnly("Search the data dictionary"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schemaSearchIn) (*mcp.CallToolResult, schemaSearchOut, error) {
		matches, path, err := svc.SchemaSearch(ctx, in.Query, schemadict.SearchOptions{
			Limit:      in.Limit,
			TablesOnly: in.TablesOnly,
		})
		if err != nil {
			return fail("%v", err), schemaSearchOut{}, nil
		}

		out := schemaSearchOut{Source: path, Query: in.Query, Matches: matches, Note: dictNote}

		var b strings.Builder
		fmt.Fprintf(&b, "%d match(es) for %q in %s\n", len(matches), in.Query, path)
		for _, m := range matches {
			if m.Column == nil {
				fmt.Fprintf(&b, "\n[table] %s  (%s)\n", m.Table, m.System)
				continue
			}
			c := m.Column
			key := ""
			if c.KeyMarked {
				key = "  [key-marked]"
			}
			fmt.Fprintf(&b, "\n%s.%s  %s\n  %s  %s%s\n", m.Table, c.Name, c.Description, c.Type, m.Field, key)
		}
		if len(matches) == 0 {
			b.WriteString("\nNothing matched. The dictionary indexes identifiers and Chinese descriptions; " +
				"an English business term will usually miss.\n")
		}
		fmt.Fprintf(&b, "\nnote: %s\n", dictNote)
		return text(b.String()), out, nil
	})
}
