// Command hrm-sql-mcp exposes a guarded, read-mostly SQL Server surface over
// MCP, plus the same capabilities as plain CLI subcommands.
//
// The CLI is not an afterthought. Everything this tool can do must be runnable
// from a terminal, so no capability ends up trapped inside an editor and so
// the audit report can be regenerated from CI.
package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/codex-k8s/hrm-sql-mcp/internal/config"
	"github.com/codex-k8s/hrm-sql-mcp/internal/policy"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "hrm-sql-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "targets":
		return cmdTargets(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q (try: help)", args[0])
	}
}

func usage() error {
	fmt.Println(`hrm-sql-mcp — guarded SQL Server access for AI agents

  targets    Show declared targets and whether each passes the guard

Environment:
  HRM_SQL_MCP_PROFILE       required, one of: local, uat  (no default)
  HRM_SQL_MCP_POLICY        policy file (default mcp/hrm-sql.yaml)
  HRM_SQL_MCP_CREDENTIALS   0600 file with the two logins`)
	return nil
}

// newRegistry wires config, policy and credentials together.
func newRegistry() (*target.Registry, *policy.Policy, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if cfg.Profile == "" {
		return nil, nil, fmt.Errorf("HRM_SQL_MCP_PROFILE is not set; it has no default so that " +
			"pointing at an environment is always a deliberate act")
	}
	pol, err := policy.Load(cfg.PolicyPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := config.LoadCredentials(cfg.CredentialsPath)
	if err != nil {
		return nil, nil, err
	}
	reg, err := target.NewRegistry(pol, cfg.Profile, store.Credentials())
	if err != nil {
		return nil, nil, err
	}
	return reg, pol, nil
}

// cmdTargets is the first thing anyone — human or agent — should run.
// Knowing exactly which server and database you are about to touch removes
// the single most common cause of destructive mistakes.
func cmdTargets(_ []string) error {
	reg, pol, err := newRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()

	ctx := context.Background()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROFILE\t%s\n\n", pol.Profile)
	fmt.Fprintln(w, "ALIAS\tSERVER\tDATABASE\tWRITABLE\tGUARD\tCONNECT")

	for _, alias := range reg.Aliases() {
		t, gerr := reg.Check(ctx, alias, target.ReadOnly)
		if gerr != nil {
			fmt.Fprintf(w, "%s\t-\t-\t-\tREJECTED\t%v\n", alias, gerr)
			continue
		}
		pt, _ := pol.TargetByAlias(alias)
		status := "ok"
		db, _, oerr := reg.Open(ctx, alias, target.ReadOnly)
		if oerr != nil {
			status = "FAIL: " + oerr.Error()
		} else if perr := db.PingContext(ctx); perr != nil {
			status = "FAIL: " + perr.Error()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\tpass\t%s\n",
			t.Alias(), t.Addr(), t.Database(), pt.Writable, status)
	}
	return w.Flush()
}
