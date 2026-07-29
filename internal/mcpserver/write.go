package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/sqlrun"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
	"github.com/codex-k8s/hrm-sql-mcp/internal/tsql"
)

// AddWriteTools registers the tools that can change the database.
//
// Separate from New, and called separately, so that "which tools can write" is
// a decision someone makes at a call site rather than a property of a list
// that grows. The Gemini registration passes an allowlist of the read-only
// tools; nothing here is in it.
func AddWriteTools(srv *mcp.Server, svc *service.Service) {
	addExecute(srv, svc)
	addSPDeploy(srv, svc)
}

// destructive marks a tool that changes things, so a client can style the
// confirmation differently from a read.
func destructive(title string) *mcp.ToolAnnotations {
	yes, no := true, false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: &yes,
		// Re-running a write is not free, whatever the statement looks like.
		IdempotentHint: false,
		OpenWorldHint:  &no,
	}
}

// approvalDoc is repeated in both write tools' descriptions because it
// describes the calling protocol, and an agent that misses it will read the
// first response as a failure and give up.
const approvalDoc = "TWO-STEP for commit=true: call once without approval_id to get an approval_id back, " +
	"tell the user to run `hrm-sql-mcp approve <id>` in a terminal, then call again with that approval_id. " +
	"The approval is bound to this exact statement text — changing so much as a space invalidates it. " +
	"commit=false needs no approval: it runs the statement and rolls it back."

type executeIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets. Must be one the policy marks writable."`
	SQL    string `json:"sql" jsonschema:"The T-SQL to run."`
	Commit bool   `json:"commit,omitempty" jsonschema:"false (default) runs inside a transaction and rolls back, reporting what would have changed. true commits and requires approval."`
	// Named approval_id rather than "token" on purpose: it is a reference to a
	// decision somebody made, not a secret that grants access.
	ApprovalID string `json:"approval_id,omitempty" jsonschema:"The id returned by a previous call, once a human has approved it."`
	Reason     string `json:"reason,omitempty" jsonschema:"Why this change is needed. Shown to whoever approves it."`
}

type writeOut struct {
	Target       targetEnvelope `json:"target"`
	RowsAffected int64          `json:"rows_affected"`
	Committed    bool           `json:"committed"`
	ElapsedMS    int64          `json:"elapsed_ms"`
	Classified   string         `json:"classified,omitempty"`
	ApprovalID   string         `json:"approval_id,omitempty"`
	SnapshotPath string         `json:"snapshot_path,omitempty"`
	Caveats      []string       `json:"caveats,omitempty"`
}

func addExecute(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_execute",
		Description: "Run a statement that changes data, against the read-write login. " +
			"Default is a rehearsal: the statement runs inside a transaction and is rolled back, " +
			"so you can see how many rows it would touch without changing anything. " + approvalDoc,
		Annotations: destructive("Change data (approval required to commit)"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in executeIn) (*mcp.CallToolResult, writeOut, error) {
		res, t, err := svc.Execute(ctx, in.Target, in.SQL, service.WriteOptions{
			Commit:     in.Commit,
			ApprovalID: in.ApprovalID,
			Reason:     in.Reason,
		})
		if err != nil {
			return writeError(err, in.SQL), writeOut{}, nil
		}
		out := writeResult(svc, t, res)
		return text(renderWrite("mssql_execute", out, res)), out, nil
	})
}

type spDeployIn struct {
	Target string `json:"target,omitempty" jsonschema:"Target alias from mssql_targets. Must be one the policy marks writable."`
	Name   string `json:"name" jsonschema:"Procedure name, bare or schema-qualified. Used to save the current definition before it is replaced."`
	// The whole CREATE/ALTER text, not a diff: the server stores the last
	// CREATE or ALTER verbatim, and anything less than the full text would
	// leave the result depending on what was already there.
	Definition string `json:"definition" jsonschema:"The complete CREATE PROCEDURE or ALTER PROCEDURE statement."`
	Commit     bool   `json:"commit,omitempty" jsonschema:"false (default) compiles and rolls back. true deploys and requires approval."`
	ApprovalID string `json:"approval_id,omitempty" jsonschema:"The id returned by a previous call, once a human has approved it."`
	Reason     string `json:"reason,omitempty" jsonschema:"Why this deployment is needed. Shown to whoever approves it."`
}

func addSPDeploy(srv *mcp.Server, svc *service.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "mssql_sp_deploy",
		Description: "Create or replace a stored procedure. The current definition is read and saved to a " +
			"snapshot file BEFORE anything is replaced, and the snapshot path is returned — for many of these " +
			"procedures the server's copy is the only one that exists. " + approvalDoc,
		Annotations: destructive("Deploy a stored procedure (approval required to commit)"),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in spDeployIn) (*mcp.CallToolResult, writeOut, error) {
		res, t, err := svc.SPDeploy(ctx, in.Target, in.Name, in.Definition, service.WriteOptions{
			Commit:     in.Commit,
			ApprovalID: in.ApprovalID,
			Reason:     in.Reason,
		})
		if err != nil {
			return writeError(err, in.Definition), writeOut{}, nil
		}
		out := writeResult(svc, t, res)
		return text(renderWrite("mssql_sp_deploy", out, res)), out, nil
	})
}

func writeResult(svc *service.Service, t *target.Target, res *service.WriteResult) writeOut {
	out := writeOut{
		Target:       envelope(svc.Describe(t)),
		Classified:   res.Label.Summary(),
		ApprovalID:   res.ApprovalID,
		SnapshotPath: res.SnapshotPath,
	}
	if res.ExecResult != nil {
		out.RowsAffected = res.RowsAffected
		out.Committed = res.Committed
		out.ElapsedMS = res.ElapsedMS
		out.Caveats = res.Caveats
	}
	return out
}

func renderWrite(tool string, out writeOut, res *service.WriteResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", out.Target.header())
	if out.Committed {
		fmt.Fprintf(&b, "%s COMMITTED — %d row(s) changed in %d ms\n", tool, out.RowsAffected, out.ElapsedMS)
	} else {
		fmt.Fprintf(&b, "%s REHEARSAL — rolled back. It would have changed %d row(s) (%d ms).\n",
			tool, out.RowsAffected, out.ElapsedMS)
	}
	fmt.Fprintf(&b, "classified as: %s\n", out.Classified)
	if len(res.Label.Objects) > 0 {
		fmt.Fprintf(&b, "objects: %s\n", strings.Join(res.Label.Objects, ", "))
	}
	if out.SnapshotPath != "" {
		fmt.Fprintf(&b, "previous definition saved to: %s\n", out.SnapshotPath)
	}
	if len(out.Caveats) > 0 {
		b.WriteString("\nWhat this rehearsal does NOT prove:\n")
		for _, c := range out.Caveats {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	return b.String()
}

// writeError turns the approval handshake into something an agent can act on.
//
// The first call of a commit always "fails" with approval required, and that
// is the normal path rather than an error — so the message says exactly what
// to do next instead of reading as a breakage to work around.
func writeError(err error, statement string) *mcp.CallToolResult {
	var need *service.ErrApprovalRequired
	if errors.As(err, &need) {
		label := tsql.Classify(statement)
		return fail("APPROVAL REQUIRED — nothing has run.\n\n"+
			"approval_id: %s\n"+
			"Ask the user to run this in a terminal:\n\n    hrm-sql-mcp approve %s\n\n"+
			"Then call this tool again with approval_id set to that id and the SAME sql text.\n"+
			"The statement is classified as: %s\n"+
			"Pending requests are in %s",
			need.ID, need.ID, label.Summary(), need.Dir)
	}
	if errors.Is(err, sqlrun.ErrNoWriteCredential) {
		return fail("This target is not writable: %v\n"+
			"Writability is set in the project policy and is not something this tool can change.", err)
	}
	return queryError(err)
}
