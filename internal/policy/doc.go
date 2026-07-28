// Package policy loads and validates the per-project policy file that declares
// which databases this tool may reach and what limits apply.
//
// The policy lives in the *consuming project's* repository (for HRM:
// mcp/hrm-sql.yaml), not in this one. That split is deliberate — the engine is
// generic and reusable, while "which servers may be touched from this project"
// is a fact about the project and belongs under its code review.
//
// The policy never contains credentials. Passwords come from a 0600 file
// outside both repositories; see package config.
package policy
