// Package service is the single implementation of everything this tool can
// do. The CLI and the MCP server are both thin shells over it.
//
// The split exists for one reason: auditing. Every operation here records
// itself, so a new front end cannot forget to. The alternative — a CLI that
// opens targets and audits, and an MCP server that does its own version of the
// same — has a specific failure built into it, where one path gains a feature
// or loses a record and nobody notices, because both still work.
//
// It also keeps the plan's promise that no capability ends up trapped inside
// an editor. A tool an agent can call and a person cannot is one nobody can
// check.
package service
