// Command hrm-sql-mcp exposes a guarded, read-mostly SQL Server surface over
// MCP, plus the same capabilities as plain CLI subcommands.
//
// The CLI is not an afterthought. Everything this tool can do must be runnable
// from a terminal, so no capability ends up trapped inside an editor and so
// the audit report can be regenerated from CI. Both front ends call the same
// service package; neither has logic of its own.
package main

import (
	"fmt"
	"os"
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
	case "serve":
		return cmdServe(args[1:])
	case "targets":
		return cmdTargets(args[1:])
	case "query":
		return cmdQuery(args[1:])
	case "sp":
		return cmdSP(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q (try: help)", args[0])
	}
}

func usage() error {
	fmt.Println(`hrm-sql-mcp — guarded SQL Server access for AI agents

  serve                Run as an MCP server over stdio (how editors invoke it)
  targets              Show declared targets and whether each passes the guard
  query <sql|->        Run a statement and print bounded results ("-" reads stdin)
  sp list              List the procedures the database actually has
  sp get <name>        Print one procedure's definition from the database
  sp diff [name...]    Compare scripts against the database
  sp audit             Three-way audit: files x database x Java call sites

Environment:
  HRM_SQL_MCP_PROFILE       required, one of: local, uat  (no default)
  HRM_SQL_MCP_POLICY        policy file (default mcp/hrm-sql.yaml)
  HRM_SQL_MCP_CREDENTIALS   0600 file with the two logins
  HRM_SQL_MCP_PROJECT_ROOT  resolves the policy's relative paths (default .)
  HRM_SQL_MCP_ACTOR         who is driving; recorded in every audit line`)
	return nil
}
