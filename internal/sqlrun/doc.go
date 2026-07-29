// Package sqlrun executes SQL against a guard-approved target and bounds the
// result before it reaches the caller.
//
// Three bounds are applied, and each one exists because of a distinct way an
// agent-driven query goes wrong:
//
//   - A context deadline. An agent that writes an accidental cross join will
//     otherwise sit there until the client gives up, with the server still
//     working. go-mssqldb turns a cancelled context into a TDS attention, so
//     the server stops too rather than finishing the query into a void.
//   - SET LOCK_TIMEOUT. A deadline alone does not distinguish "this query is
//     expensive" from "this query is stuck behind someone else's transaction".
//     A short lock timeout turns the second case into an immediate, legible
//     error (1222) instead of a timeout that reads like a slow query.
//   - Client-side row and byte caps. The server has no idea how much text the
//     caller can absorb; a SELECT * over a payroll table is a perfectly valid
//     query with a completely unusable answer.
//
// What this package deliberately does NOT do is inspect the SQL and refuse
// statements it thinks are writes. The boundary that stops writes is the
// hrm_mcp_ro login's DENY, enforced by SQL Server. A client-side classifier in
// front of that would only obscure it: if the DENY were ever missing, the
// classifier would quietly cover for it and the tests proving "this login
// cannot write" would pass for the wrong reason. Statement classification
// arrives later, for labelling audit records, not for gating.
package sqlrun
