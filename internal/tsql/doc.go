// Package tsql labels a statement so a human reading the audit log or an
// approval prompt can see at a glance what it is.
//
// # This is not an authorisation mechanism, and must never become one
//
// T-SQL makes string-based authorisation unachievable, not merely difficult.
// EXEC(@sql) and sp_executesql build statements at run time out of values this
// package never sees, so any classifier can be handed a DELETE it will happily
// label as a procedure call. Measured on this project: hrm_mcp_ro is refused
// EXEC sp_executesql N'DELETE …' with error 229 — the permission stopped what
// no amount of parsing could have.
//
// The danger of a classifier is not that it is imperfect. It is that a good
// one looks like a control. Once something in the code path says "this looks
// like a read, let it through", the DENY behind it stops being tested, and the
// day it goes missing everything still appears to work. So this package
// returns a label and nothing else: no Allow, no IsSafe, no boolean any caller
// could branch on to skip a check.
//
// What the label is for: reading the log, and telling a person about to
// approve something what they are approving.
package tsql
