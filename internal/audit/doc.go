// Package audit records every database operation to an append-only JSONL file.
//
// Two properties of how this tool runs decide the whole design.
//
// It is multi-process. MCP uses stdio, so each client spawns its own
// hrm-sql-mcp: Claude Code has one, Gemini CLI has another, and a terminal
// invocation is a third. They share one policy, one credentials file, and one
// audit log. Anything held in memory — the slog-based recorder this package
// replaces, or an in-memory pending-approval map — is per-process state
// pretending to be shared state, and the failure only shows up when two agents
// are actually working at once.
//
// And it is the record of what an autonomous agent did to a payroll database.
// That makes losing a line worse than any cost of writing one. Hence
// append-only: no rewriting, no rotation inside the process, no buffering that
// a kill -9 could discard.
//
// The concurrency mechanism is O_APPEND plus small records. Under O_APPEND the
// kernel makes the seek-to-end and the write one operation, so two processes
// cannot overwrite each other's bytes. What that alone does not rule out is a
// short write, which would let one record split around another; Go's
// os.File.Write retries on short writes rather than failing, so a split would
// be silent. Keeping every line below PIPE_BUF (4096) is what closes that gap,
// and MaxLine is enforced rather than assumed — a record that cannot be made
// to fit is refused, not written and hoped for.
//
// Records describe completed operations. A process killed mid-query leaves no
// line, which is acceptable while every tool is read-only. It will not be once
// writes exist: the write path must record its intent before acting, which is
// why Event carries a correlation ID from the start — two lines sharing one ID
// is all that takes, and no new machinery.
package audit
