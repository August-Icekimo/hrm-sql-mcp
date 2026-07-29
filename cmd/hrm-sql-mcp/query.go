package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias (required when the policy declares more than one)")
	format := fs.String("format", "table", "output format: table or json")
	maxRows := fs.Int("max-rows", 0, "override the policy row cap (downwards only)")
	timeout := fs.Duration("timeout", 0, "override the policy query timeout (downwards only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	statement, err := readStatement(fs.Args())
	if err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	ctx := context.Background()
	res, t, err := svc.Query(ctx, *alias, statement, service.QueryOptions{
		MaxRows: *maxRows,
		Timeout: *timeout,
	})
	if err != nil {
		if n, ok := sqlrun.ServerErrorNumber(err); ok {
			return fmt.Errorf("SQL Server error %d: %w", n, err)
		}
		return err
	}

	if *format == "json" {
		return printJSON(svc, t, statement, res)
	}
	return printTable(svc, t, res)
}

// readStatement takes the SQL from the command line, or from stdin for "-".
func readStatement(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("no statement given (pass SQL as an argument, or \"-\" to read stdin)")
	}
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	return strings.Join(args, " "), nil
}

func printJSON(svc *service.Service, t *target.Target, statement string, res *sqlrun.Result) error {
	out := map[string]any{
		"target":    svc.Describe(t),
		"statement": statement,
		"result":    res,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printTable(svc *service.Service, t *target.Target, res *sqlrun.Result) error {
	// The header goes to stderr so that piping stdout into a file or another
	// tool yields just the data, while a human still sees which database the
	// numbers came from. Getting that pairing wrong is how a UAT figure ends up
	// quoted as production.
	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s @ %s/%s as %s (%s), %d ms\n",
		d["alias"], d["server"], d["database"], d["login"], d["mode"], res.ElapsedMS)

	for i, set := range res.Sets {
		if len(set.Columns) == 0 {
			continue
		}
		if i > 0 {
			fmt.Println()
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		names := make([]string, len(set.Columns))
		for j, c := range set.Columns {
			names[j] = c.Name
		}
		fmt.Fprintln(w, strings.Join(names, "\t"))
		for _, row := range set.Rows {
			cells := make([]string, len(row))
			for j, v := range row {
				cells[j] = cell(v)
			}
			fmt.Fprintln(w, strings.Join(cells, "\t"))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "-- %d row(s)", len(set.Rows))
		if set.Truncated {
			fmt.Fprintf(os.Stderr, ", TRUNCATED by %s", set.TruncatedBy)
		}
		fmt.Fprintln(os.Stderr)
	}
	return nil
}

// cell renders one value for the terminal.
//
// NULL is spelled out rather than shown as an empty cell: an empty string and
// a NULL mean very different things in a payroll table, and a report that
// cannot tell them apart is worse than one that is harder to read.
func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return strings.ReplaceAll(x, "\t", " ")
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}
