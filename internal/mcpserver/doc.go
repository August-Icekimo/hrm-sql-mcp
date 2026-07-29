// Package mcpserver exposes the service over MCP.
//
// It is deliberately thin. Every tool here resolves arguments, calls one
// service method, and renders the answer — no guard checks, no limits, no
// audit calls of its own. Anything this layer decided for itself would be a
// rule that applies to agents and not to the CLI, and the first time those two
// disagreed, the one nobody runs by hand would be the one that was wrong.
//
// Two things shape the tool surface.
//
// Every tool takes an explicit target and every response repeats which server,
// database and login answered. An agent holds no memory of how the process was
// configured, and a result that does not say where it came from is one it can
// only guess about — that guess is how a UAT number gets reported as
// production.
//
// And the responses are written for a reader that cannot ask a follow-up
// question. A truncated result says it was truncated and why; a refusal says
// whether it came from the guard or from SQL Server. An agent given "error"
// with no shape will retry, and retrying a permission denial against a payroll
// database is the wrong reflex to design in.
package mcpserver
