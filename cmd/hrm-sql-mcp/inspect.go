package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/codex-k8s/hrm-sql-mcp/internal/schemadict"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
)

func cmdExplain(args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	withXML := fs.Bool("xml", false, "print the raw showplan XML as well")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
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

	plan, t, err := svc.Explain(context.Background(), *alias, statement, *withXML)
	if err != nil {
		return err
	}

	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s @ %s/%s — 估算值，語句沒有被執行\n", d["alias"], d["server"], d["database"])

	for _, s := range plan.Statements {
		fmt.Printf("%s\n  估算列數 %.0f，成本 %.4f\n\n", oneLine(s.Text), s.EstimatedRows, s.EstimatedCost)
	}
	if scans := plan.Scans(); len(scans) > 0 {
		fmt.Println("SCAN（會隨資料量成長）")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, o := range scans {
			fmt.Fprintf(w, "  %s\t%s\t估算列數 %.0f\t子樹成本 %.4f\n", o.Physical, o.Logical, o.EstimatedRows, o.EstimatedCost)
		}
		w.Flush()
		fmt.Println()
	}
	for _, warn := range plan.Warnings {
		fmt.Printf("⚠ %s %s\n", warn.Kind, warn.Detail)
	}
	for _, mi := range plan.MissingIndexes {
		fmt.Printf("建議索引 %s（影響 %.1f%%）equality=%v inequality=%v include=%v\n",
			mi.Table, mi.Impact, mi.Equality, mi.Inequality, mi.Include)
	}
	fmt.Printf("\n總估算成本 %.4f（計畫 XML %d bytes）\n", plan.TotalCost(), plan.XMLBytes)
	if *withXML {
		fmt.Println(plan.XML)
	}
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cmdDeps(args []string) error {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	dir := fs.String("direction", "both", "uses, used_by or both")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("deps needs exactly one object name")
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	deps, t, err := svc.Deps(context.Background(), *alias, fs.Arg(0), spdb.Direction(*dir))
	if err != nil {
		return err
	}

	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s @ %s/%s — %s\n", d["alias"], d["server"], d["database"], fs.Arg(0))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, dep := range deps {
		arrow := "→"
		if dep.Direction == spdb.UsedBy {
			arrow = "←"
		}
		flags := ""
		if !dep.Exists {
			flags += "  ⚠ 資料庫中不存在"
		}
		if dep.CallerDependent {
			flags += "  [執行期才決定]"
		}
		if dep.Ambiguous {
			flags += "  [無法唯一解析]"
		}
		fmt.Fprintf(w, "%s\t%s\t%s%s\n", arrow, dep.Name, dep.Type, flags)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Printed every time, not only when the list is empty. The caveat matters
	// most when the answer looks complete.
	fmt.Fprintln(os.Stderr, "-- 只涵蓋 SQL 對 SQL 的引用。動態組出來的名稱（EXEC(@sql)）看不到，"+
		"應用程式的呼叫點也不在這裡——那要用 sp audit。")
	return nil
}

func cmdSchema(args []string) error {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	tablesOnly := fs.Bool("tables", false, "只比對資料表名稱與所屬系統")
	limit := fs.Int("limit", 50, "最多回傳幾筆")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("schema needs a search term")
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	query := strings.Join(fs.Args(), " ")
	matches, path, err := svc.SchemaSearch(context.Background(), query,
		schemadict.SearchOptions{Limit: *limit, TablesOnly: *tablesOnly})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "-- %d 筆符合 %q，來源 %s\n", len(matches), query, path)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, m := range matches {
		if m.Column == nil {
			fmt.Fprintf(w, "[表]\t%s\t%s\t\n", m.Table, m.System)
			continue
		}
		key := ""
		if m.Column.KeyMarked {
			key = " [有鍵標記]"
		}
		fmt.Fprintf(w, "%s.%s\t%s\t%s\t%s%s\n",
			m.Table, m.Column.Name, m.Column.Description, m.Column.Type, m.Field, key)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "-- 來自版控裡的資料字典，不是線上資料庫；可能過期，主鍵欄位不可靠。")
	return nil
}
