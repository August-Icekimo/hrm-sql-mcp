package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codex-k8s/hrm-sql-mcp/internal/approver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
)

func cmdApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	list := fs.Bool("list", false, "列出待核可的請求")
	deny := fs.Bool("deny", false, "拒絕而不是核可")
	reason := fs.String("reason", "", "決定的理由，會寫進決定檔")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkFlagsFirst(fs.Args()); err != nil {
		return err
	}

	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()
	store := svc.Approvals()

	if *list || fs.NArg() == 0 {
		return listPending(store)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("approve 一次只處理一個 id")
	}

	id := fs.Arg(0)
	req, err := store.Get(id)
	if err != nil {
		return err
	}

	// The statement is printed in full before the decision is recorded.
	// Approving something you have not read is the failure this whole gate
	// exists to prevent, so the text is not summarised or truncated here.
	printRequest(os.Stdout, req)

	decision := approver.DecisionApprove
	if *deny {
		decision = approver.DecisionDeny
	}
	by := os.Getenv("USER")
	if by == "" {
		by = "unknown"
	}
	if err := store.Decide(id, decision, by, *reason); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n-- 已記錄：%s（by %s）\n", decision, by)
	if decision == approver.DecisionApprove {
		fmt.Fprintln(os.Stderr, "-- 回到 agent，用同一個 approval_id 與完全相同的語句再呼叫一次。")
	}
	return nil
}

func listPending(store *approver.Store) error {
	pending, err := store.Pending()
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintf(os.Stderr, "-- 沒有待核可的請求（%s）\n", store.Dir())
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTOOL\tTARGET\t分類\t剩餘時間\t語句（前 60 字）")
	for _, r := range pending {
		left := time.Until(r.Expires).Round(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s/%s\t%s\t%s\t%s\n",
			r.ID, r.Tool, r.Alias, r.Database, r.Summary, left, oneLineClip(r.Statement, 60))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n-- 用 `hrm-sql-mcp approve <id>` 看完整語句並決定。\n")
	return nil
}

func printRequest(w *os.File, r approver.Request) {
	fmt.Fprintf(w, "──────────────────────────────────────────────────────────────\n")
	fmt.Fprintf(w, "核可請求 %s\n", r.ID)
	fmt.Fprintf(w, "  工具      %s\n", r.Tool)
	fmt.Fprintf(w, "  發起者    %s\n", r.Actor)
	fmt.Fprintf(w, "  目標      %s  %s/%s  以 %s 身分\n", r.Alias, r.Server, r.Database, r.Login)
	fmt.Fprintf(w, "  分類      %s\n", r.Summary)
	if len(r.Objects) > 0 {
		fmt.Fprintf(w, "  牽涉物件  %s\n", strings.Join(r.Objects, ", "))
	}
	if r.Rehearsal != "" {
		fmt.Fprintf(w, "  理由      %s\n", r.Rehearsal)
	}
	fmt.Fprintf(w, "  逾期      %s\n", r.Expires.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "──────────────────────────────────────────────────────────────\n")
	fmt.Fprintf(w, "%s\n", r.Statement)
	fmt.Fprintf(w, "──────────────────────────────────────────────────────────────\n")
}

func oneLineClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
