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
	fmt.Fprintln(w, "ALIAS\tSERVER\tDATABASE\tWRITABLE\tGUARD\tCONNECT\tSNAPSHOT")
	for _, t := range targets {
		if t.Guard != "pass" {
			fmt.Fprintf(w, "%s\t-\t-\t-\tREJECTED\t%s\t\n", t.Alias, t.Reason)
			continue
		}
		snap := "-"
		if t.Snapshot != nil {
			snap = t.Snapshot.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\tpass\t%s\t%s\n",
			t.Alias, t.Server, t.Database, t.Writable, t.Connect, snap)
	}
	if err := w.Flush(); err != nil {
		return err
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
