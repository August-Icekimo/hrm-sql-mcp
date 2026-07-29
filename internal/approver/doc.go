// Package approver gates committed writes behind a human decision.
//
// # Why files, and not a prompt
//
// Under MCP the client owns stdin, so there is no terminal to prompt on: the
// process cannot ask a question and cannot read an answer. And there is more
// than one process — Claude Code, Gemini CLI and a terminal each spawn their
// own — so an in-memory pending map is per-process state pretending to be
// shared, and the approval a person granted in one would be invisible to the
// one that asked.
//
// So a request is a file, a decision is a file, and any process can write
// either. The asking process polls for the decision; a person runs
// `hrm-sql-mcp approve <id>` from any terminal, at any time, against any of
// the running servers.
//
// # What the gate is and is not
//
// It stops an agent writing without a person having said yes to that specific
// statement. It does not stop a person who wants to write; nothing here is a
// defence against the operator. The database permissions are what bound the
// damage once approval is given.
//
// Three properties are deliberate:
//
//   - Every commit is approved separately, with no caching. A cached approval
//     is an approval for a statement nobody read.
//   - The decision names the exact statement it approved, and the asking
//     process checks that the statement it is about to run is still that one.
//     Otherwise an approval becomes a token that any later request can spend.
//   - Requests expire. A pending approval nobody answered must not sit on disk
//     waiting to authorise something days later.
package approver
