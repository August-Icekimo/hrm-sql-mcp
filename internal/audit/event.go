package audit

import (
	"time"

	"github.com/google/uuid"
)

// Outcomes recorded in Event.Outcome.
const (
	// OutcomeOK means the operation completed.
	OutcomeOK = "ok"
	// OutcomeError means it failed after being attempted.
	OutcomeError = "error"
	// OutcomeDenied means this tool refused before reaching the database —
	// a guard rejection, a missing credential, an unwritable target.
	//
	// Kept distinct from "error" because the two answer different questions.
	// "error" is the database's verdict; "denied" is ours, and a run of them
	// is either a misconfiguration or somebody probing at the guard.
	OutcomeDenied = "denied"
)

// Event is one audited operation.
//
// The field set is fixed rather than a free-form map. An audit log is read
// months later by something that has to parse it — a grep, a jq filter, the
// CI check — and a schema that varies per call site cannot be relied on. New
// facts should become new fields here, where the cost of adding one is visible.
type Event struct {
	// Time is when the operation completed.
	Time time.Time `json:"time"`
	// CorrelationID links records that describe the same operation. UUID
	// rather than a counter because the processes writing here cannot see
	// each other's numbering.
	CorrelationID string `json:"correlation_id"`
	// PID identifies the writing process, which is what makes an interleaving
	// bug diagnosable after the fact rather than theoretical.
	PID int `json:"pid"`
	// Actor is who drove the tool: cli, claude-code, gemini.
	Actor string `json:"actor,omitempty"`
	// Tool is the operation name, matching the MCP tool where there is one.
	Tool string `json:"tool"`

	// The target, spelled out on every line. A log that says "500 rows read"
	// without saying from which server answers nothing.
	Alias    string `json:"alias,omitempty"`
	Server   string `json:"server,omitempty"`
	Database string `json:"database,omitempty"`
	// Login is the SQL login used, so a DBA's "who did this" has an answer
	// that does not depend on our word for it.
	Login string `json:"login,omitempty"`
	Mode  string `json:"mode,omitempty"`

	// Statement is the SQL as submitted. It may be clipped; see Clipped.
	Statement string `json:"statement,omitempty"`

	Outcome   string `json:"outcome"`
	Rows      int    `json:"rows,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
	// Truncated reports that the *result* was cut by a limit, which is
	// unrelated to Clipped below.
	Truncated bool `json:"truncated,omitempty"`
	// ServerError is the SQL Server error number, present when the database
	// refused. 229 (permission denied) is the one worth alerting on.
	ServerError int32 `json:"server_error,omitempty"`
	// Error is the failure message.
	Error string `json:"error,omitempty"`

	// Clipped names the fields shortened to fit the line-length bound, so a
	// reader can tell a short statement from a shortened one. Without it, a
	// clipped record is indistinguishable from an accurate one.
	Clipped []string `json:"clipped,omitempty"`
}

// NewID returns a correlation ID.
func NewID() string { return uuid.NewString() }
