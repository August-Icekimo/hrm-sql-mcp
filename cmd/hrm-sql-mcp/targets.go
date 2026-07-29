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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROFILE\t%s\n\n", svc.Policy().Profile)
	fmt.Fprintln(w, "ALIAS\tSERVER\tDATABASE\tWRITABLE\tGUARD\tCONNECT")
	for _, t := range svc.Targets(context.Background()) {
		if t.Guard != "pass" {
			fmt.Fprintf(w, "%s\t-\t-\t-\tREJECTED\t%s\n", t.Alias, t.Reason)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\tpass\t%s\n",
			t.Alias, t.Server, t.Database, t.Writable, t.Connect)
	}
	return w.Flush()
}
