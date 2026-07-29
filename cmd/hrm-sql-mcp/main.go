// Command hrm-sql-mcp exposes a guarded, read-mostly SQL Server surface over
// MCP, plus the same capabilities as plain CLI subcommands.
//
// The CLI is not an afterthought. Everything this tool can do must be runnable
// from a terminal, so no capability ends up trapped inside an editor and so
// the audit report can be regenerated from CI. Both front ends call the same
// service package; neither has logic of its own.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Exit codes. A CI step needs to tell "the tool broke" from "the tool worked
// and the answer is bad", and those two demand different responses from
// whoever is looking at the failure.
const (
	exitFindings = 2 // the audit found something a human must look at
	exitStale    = 3 // a committed report no longer matches the facts
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "hrm-sql-mcp: %v\n", err)
		code := 1
		var ec *exitError
		if errors.As(err, &ec) {
			code = ec.code
		}
		os.Exit(code)
	}
}

// exitError carries a specific exit code out of a subcommand.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func failWith(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
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
	case "explain":
		return cmdExplain(args[1:])
	case "deps":
		return cmdDeps(args[1:])
	case "schema":
		return cmdSchema(args[1:])
	case "execute":
		return cmdExecute(args[1:])
	case "approve":
		return cmdApprove(args[1:])
	case "sp":
		return cmdSP(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q (try: help)", args[0])
	}
}

// checkFlagsFirst rejects a flag that appears after a positional argument.
//
// Go's flag package stops parsing at the first non-flag word, so
// `schema 特休 --limit 6` silently searches for the literal string
// "特休 --limit 6" and reports no matches. The user gets a wrong answer that
// looks like a real one. Refusing outright costs a retype; not refusing costs
// somebody concluding a column does not exist.
func checkFlagsFirst(args []string) error {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			return fmt.Errorf("flags must come before positional arguments; move %q ahead of them", a)
		}
	}
	return nil
}

func usage() error {
	fmt.Println(`hrm-sql-mcp — guarded SQL Server access for AI agents

  serve                Run as an MCP server over stdio (how editors invoke it)
  targets              Show declared targets and whether each passes the guard
  query <sql|->        Run a statement and print bounded results ("-" reads stdin)
  explain <sql|->      Estimated plan for a statement, without running it
  deps <name>          What an object references, and what references it
  schema <term>        Search the data dictionary by name or Chinese description
  sp list              List the procedures the database actually has
  sp get <name>        Print one procedure's definition from the database
  sp diff [name...]    Compare scripts against the database
  sp audit             Three-way audit: files x database x Java call sites
                       --offline drops to files x Java and never connects;
                       --gate exits 2 on findings, --check exits 3 if stale
  sp deploy --name N   Create or replace a procedure (snapshots the old one first)

Write path (needs an approval; rehearses and rolls back by default):
  execute <sql|->      Run a data change. Add --commit --approval <id> to make it stick
  approve [id]         List pending approvals, or read one and decide

Environment:
  HRM_SQL_MCP_PROFILE       required, one of: local, uat  (no default)
  HRM_SQL_MCP_POLICY        policy file (default mcp/hrm-sql.yaml)
  HRM_SQL_MCP_CREDENTIALS   0600 file with the two logins
  HRM_SQL_MCP_PROJECT_ROOT  resolves the policy's relative paths (default .)
  HRM_SQL_MCP_ACTOR         who is driving; recorded in every audit line
  HRM_SQL_MCP_ENV_FILE      extra dotenv files, comma separated

Every policy field can be overridden per target, and "targets" prints whatever
was overridden and from where:

  HRM_SQL_TARGET_<ALIAS>_{HOST,PORT,DATABASE,ALLOW_CIDRS,WRITABLE,...}
  HRM_SQL_MCP_EXTRA_TARGETS   declare targets the policy file does not have

None of it reaches the compile-time production denylist. See the README.`)
	return nil
}
