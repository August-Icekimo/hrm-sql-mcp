package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/spaudit"
)

func cmdSP(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sp needs a subcommand: list, get, diff, audit or deploy")
	}
	switch args[0] {
	case "list":
		return cmdSPList(args[1:])
	case "get":
		return cmdSPGet(args[1:])
	case "diff":
		return cmdSPDiff(args[1:])
	case "audit":
		return cmdSPAudit(args[1:])
	case "deploy":
		return cmdSPDeploy(args[1:])
	default:
		return fmt.Errorf("unknown sp subcommand %q (try: list, get, diff, audit, deploy)", args[0])
	}
}

func cmdSPList(args []string) error {
	fs := flag.NewFlagSet("sp list", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	procs, t, err := svc.SPList(context.Background(), *alias)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROCEDURE\tTYPE\tCREATED\tMODIFIED\tREADABLE")
	for _, p := range procs {
		readable := "yes"
		if p.Encrypted {
			readable = "ENCRYPTED"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Qualified(), p.Type,
			p.Created.Format("2006-01-02"), p.Modified.Format("2006-01-02"), readable)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %d procedure(s) in %s/%s\n", len(procs), d["server"], d["database"])
	return nil
}

func cmdSPGet(args []string) error {
	fs := flag.NewFlagSet("sp get", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("sp get needs exactly one procedure name")
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	p, t, err := svc.SPGet(context.Background(), *alias, fs.Arg(0))
	if err != nil {
		return err
	}
	d := svc.Describe(t)
	fmt.Fprintf(os.Stderr, "-- %s from %s/%s, last modified %s\n",
		p.Qualified(), d["server"], d["database"], p.Modified.Format("2006-01-02 15:04"))
	if p.Encrypted {
		return fmt.Errorf("%s was created WITH ENCRYPTION; its definition cannot be read", p.Qualified())
	}
	fmt.Println(p.Definition)
	return nil
}

func cmdSPDiff(args []string) error {
	fs := flag.NewFlagSet("sp diff", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	lines := fs.Int("context", 3, "diff context lines")
	all := fs.Bool("all", false, "also list procedures that match")
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}

	res, _, err := svc.SPDiff(context.Background(), *alias, fs.Args(), *lines)
	if err != nil {
		return err
	}

	counts := map[service.DiffStatus]int{}
	for _, r := range res.Results {
		counts[r.Status]++
		switch r.Status {
		case service.DiffIdentical:
			if *all {
				fmt.Printf("== %s\n", r.Name)
			}
		case service.DiffFileOnly:
			fmt.Printf("!! %s — 檔案有，資料庫沒有（%s）\n", r.Name, r.FilePath)
		case service.DiffDBOnly:
			fmt.Printf("!! %s — 資料庫有，但沒有原始檔\n", r.DBName)
		case service.DiffEncrypted:
			fmt.Printf("?? %s — 資料庫端為 WITH ENCRYPTION，無法比對\n", r.Name)
		case service.DiffDiffers:
			fmt.Printf("\n>> %s\n", r.Name)
			if len(r.OtherFiles) > 0 {
				fmt.Printf("⚠ 比對的是 %s；此程序另有 %d 個原始檔：%s\n",
					r.FilePath, len(r.OtherFiles), strings.Join(r.OtherFiles, ", "))
			}
			fmt.Print(r.Diff)
		}
	}

	fmt.Fprintf(os.Stderr, "\n-- 相同 %d，不同 %d，只有檔案 %d，只有資料庫 %d，無法比對 %d，無法解碼的檔案 %d\n",
		counts[service.DiffIdentical], counts[service.DiffDiffers], counts[service.DiffFileOnly],
		counts[service.DiffDBOnly], counts[service.DiffEncrypted], len(res.Failures))
	for _, f := range sortedFailureKeys(res.Failures) {
		fmt.Fprintf(os.Stderr, "-- 無法解碼 %s: %s\n", f, res.Failures[f])
	}
	return nil
}

func cmdSPAudit(args []string) error {
	fs := flag.NewFlagSet("sp audit", flag.ContinueOnError)
	alias := fs.String("target", "", "target alias")
	format := fs.String("format", "text", "output format: text or markdown")
	out := fs.String("out", "", "write to this file instead of stdout")
	timestamp := fs.Bool("timestamp", false, "include the generation time (off by default so a committed report only changes when the facts do)")
	diffs := fs.Bool("diffs", true, "embed diffs for differing procedures (markdown only)")
	offline := fs.Bool("offline", false, "compare files against Java only; do not open a database connection")
	check := fs.Bool("check", false, "with --out: compare instead of writing, and exit 3 if the file is out of date")
	gate := fs.Bool("gate", false, "exit 2 when the audit finds something needing a human")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *check && *out == "" {
		return fmt.Errorf("--check needs --out: there is no committed file to compare against")
	}
	if *offline && *alias != "" {
		// Silently ignoring one of them would let a pre-push hook believe it
		// audited hrm_0209 when it never opened a socket.
		return fmt.Errorf("--offline and --target contradict each other; --offline never connects to anything")
	}

	rep, err := runAudit(*offline, *alias)
	if err != nil {
		return err
	}

	var text string
	if *format == "markdown" {
		text = rep.Markdown(spaudit.MarkdownOptions{Timestamp: *timestamp, Diffs: *diffs})
	} else {
		text = auditText(rep)
	}

	switch {
	case *check:
		if err := checkReport(*out, text, rep); err != nil {
			return err
		}
	case *out == "":
		fmt.Print(text)
	default:
		if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "-- 已寫入 %s（%d 支程序）\n", *out, len(rep.Rows))
	}

	// Something to act on is called out on stderr rather than left for the
	// reader to spot in a table. Untidiness alone (orphan, file-only,
	// unreferenced) does not qualify — a warning that fires on housekeeping
	// gets ignored, and so does the gate built on it.
	if rep.HasFindings() {
		what := "ghost / differs / db-only / 解碼失敗"
		if rep.Offline {
			what = "missing-script / 解碼失敗"
		}
		msg := fmt.Sprintf("-- 有需要處理的項目（%s）", what)
		if *gate {
			return failWith(exitFindings, "%s", strings.TrimPrefix(msg, "-- "))
		}
		fmt.Fprintln(os.Stderr, msg)
	}
	return nil
}

// runAudit picks the lane. Offline uses a Service built without credentials,
// so it works on a machine that has never been given a database login.
func runAudit(offline bool, alias string) (*spaudit.Report, error) {
	if offline {
		svc, err := service.NewOffline()
		if err != nil {
			return nil, err
		}
		defer svc.Close()
		return svc.SPAuditOffline(context.Background())
	}
	svc, err := service.New()
	if err != nil {
		return nil, err
	}
	defer svc.Close()
	rep, _, err := svc.SPAudit(context.Background(), alias)
	return rep, err
}

// checkReport compares the committed document against what the facts say now.
//
// It writes nothing. A --check that repaired the file would report success on
// the run that changed it, which is the one thing a reviewer needed to see.
func checkReport(path, want string, rep *spaudit.Report) error {
	got, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return failWith(exitStale, "%s 不存在；用同一道指令去掉 --check 產生它", path)
		}
		return err
	}
	if string(got) == want {
		fmt.Fprintf(os.Stderr, "-- %s 是最新的（%d 支程序）\n", path, len(rep.Rows))
		return nil
	}
	return failWith(exitStale,
		"%s 與現況不符；用同一道指令去掉 --check 重新產生後一併提交", path)
}

// auditText is the terminal summary: counts, then the rows that need action.
func auditText(rep *spaudit.Report) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	if rep.Offline {
		// Stated first and in the same slot the server would occupy, because
		// the difference between this report and a full one is not a detail.
		fmt.Fprintf(w, "TARGET\t(離線：未查詢資料庫，只比對原始檔與 Java 呼叫點)\n")
	} else {
		fmt.Fprintf(w, "TARGET\t%s %s/%s as %s\n",
			rep.Target["alias"], rep.Target["server"], rep.Target["database"], rep.Target["login"])
	}
	fmt.Fprintf(w, "SP DIR\t%s\n", rep.SPDir)
	fmt.Fprintf(w, "JAVA\t%s (%d files)\n\n", rep.JavaDir, rep.JavaFiles)
	fmt.Fprintln(w, "STATUS\tCOUNT\tMEANING")
	for _, s := range rep.Statuses() {
		fmt.Fprintf(w, "%s\t%d\t%s\n", s, rep.Counts[s], spaudit.Explain(s))
	}
	fmt.Fprintf(w, "TOTAL\t%d\t\n", len(rep.Rows))
	w.Flush()

	detail := []spaudit.Status{spaudit.StatusGhost, spaudit.StatusDiffers, spaudit.StatusDBOnly}
	if rep.Offline {
		detail = []spaudit.Status{spaudit.StatusMissingScript}
	}
	for _, s := range detail {
		rows := rep.ByStatus(s)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s (%d)\n", strings.ToUpper(string(s)), len(rows))
		for _, r := range rows {
			switch s {
			case spaudit.StatusGhost, spaudit.StatusMissingScript:
				fmt.Fprintf(&b, "  %-32s %s\n", r.Name, firstSite(r))
			case spaudit.StatusDiffers:
				fmt.Fprintf(&b, "  %-32s %s\n", r.Name, r.FilePath)
			default:
				fmt.Fprintf(&b, "  %-32s modified %s, %d call / %d mention\n",
					r.DBName, r.DBModified.Format("2006-01-02"),
					len(r.Calls()), len(r.CallSites)-len(r.Calls()))
			}
		}
	}

	if len(rep.FileFailures) > 0 || len(rep.JavaFailures) > 0 {
		fmt.Fprintf(&b, "\nSCAN FAILURES\n")
		for _, f := range sortedFailureKeys(rep.FileFailures) {
			fmt.Fprintf(&b, "  %s: %s\n", f, rep.FileFailures[f])
		}
		for _, f := range sortedFailureKeys(rep.JavaFailures) {
			fmt.Fprintf(&b, "  %s: %s\n", f, rep.JavaFailures[f])
		}
	}
	return b.String()
}

func firstSite(r spaudit.Row) string {
	calls := r.Calls()
	if len(calls) == 0 {
		return ""
	}
	extra := ""
	if len(calls) > 1 {
		extra = fmt.Sprintf(" (+%d more)", len(calls)-1)
	}
	return fmt.Sprintf("%s:%d%s", calls[0].File, calls[0].Line, extra)
}

func sortedFailureKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
