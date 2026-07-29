package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
)

// cmdTargets is the first thing anyone — human or agent — should run.
// Knowing exactly which server and database you are about to touch removes
// the single most common cause of destructive mistakes.
func cmdTargets(_ []string) error {
	svc, err := service.New()
	if err != nil {
		return err
	}
	defer svc.Close()

	// Fetched once: each entry costs a guard check and a ping.
	targets := svc.Targets(context.Background())

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROFILE\t%s\n\n", svc.Policy().Profile)
	fmt.Fprintln(w, "ALIAS\tSERVER\tDATABASE\tWRITABLE\tGUARD\tCONNECT\tLOGIN\tSNAPSHOT")
	for _, t := range targets {
		if t.Guard != "pass" {
			fmt.Fprintf(w, "%s\t-\t-\t-\tREJECTED\t%s\t\t\n", t.Alias, t.Reason)
			continue
		}
		snap := "-"
		if t.Snapshot != nil {
			snap = t.Snapshot.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\tpass\t%s\t%s\t%s\n",
			t.Alias, t.Server, t.Database, t.Writable, t.Connect, t.CredentialFrom, snap)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Overrides are printed in full, every time, with no way to suppress them.
	// The moment host and database can come from three places, a listing that
	// showed only the resolved values would answer "what will this connect to"
	// but not "why", and the second question is the one asked at 2am.
	if ov := svc.Overrides(); len(ov) > 0 {
		fmt.Fprintf(os.Stderr, "\n-- %d 項組態被政策檔以外的來源覆寫：\n", len(ov))
		for _, o := range ov {
			fmt.Fprintf(os.Stderr, "   %s\n", o)
		}
		fmt.Fprintln(os.Stderr, "-- 編譯期的正式機黑名單不受這些覆寫影響。")
	}
	// The note says what a snapshot is for; the dates say when it is from.
	// Neither is derivable from the alias, and picking the wrong one is the
	// mistake that makes a whole comparison meaningless.
	for _, t := range targets {
		if t.Note != "" {
			fmt.Fprintf(os.Stderr, "-- %s: %s\n", t.Alias, t.Note)
		}
	}
	return nil
}
