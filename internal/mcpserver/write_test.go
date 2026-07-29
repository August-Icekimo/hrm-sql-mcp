package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/mcpserver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

// connectWrite is connect() plus the write tools, which serve registers
// separately.
func connectWrite(t *testing.T) (*mcp.ClientSession, *service.Service) {
	t.Helper()
	testenv.RequireIntegration(t)

	svc, err := service.New()
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	srv := mcpserver.New(svc)
	mcpserver.AddWriteTools(srv, svc)

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, svc
}

// TestWriteToolsAreNotInTheReadOnlyServer is the registration boundary.
// mcpserver.New is what the Gemini allowlist was written against; if a write
// tool ever appears in it, that allowlist silently stops covering everything.
func TestWriteToolsAreNotInTheReadOnlyServer(t *testing.T) {
	cs := connect(t) // New() only, no AddWriteTools
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "mssql_execute" || tool.Name == "mssql_sp_deploy" {
			t.Errorf("%s is registered by New(); write tools must only come from AddWriteTools", tool.Name)
		}
	}
}

func TestWriteToolsAreAnnotatedDestructive(t *testing.T) {
	cs, _ := connectWrite(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range res.Tools {
		if tool.Name != "mssql_execute" && tool.Name != "mssql_sp_deploy" {
			continue
		}
		found++
		if tool.Annotations == nil {
			t.Fatalf("%s has no annotations", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint {
			t.Errorf("%s claims to be read-only", tool.Name)
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("%s is not marked destructive", tool.Name)
		}
		// The two-step protocol has to be discoverable from the description,
		// or the first commit attempt reads as a plain failure.
		if !strings.Contains(tool.Description, "approve") {
			t.Errorf("%s does not describe the approval handshake", tool.Name)
		}
	}
	if found != 2 {
		t.Errorf("found %d write tools, want 2", found)
	}
}

// TestRehearsalNeedsNoApproval: requiring one for a call that changes nothing
// would train people to approve without reading.
func TestRehearsalNeedsNoApproval(t *testing.T) {
	cs, _ := connectWrite(t)
	res := call(t, cs, "mssql_execute", map[string]any{
		"target": testenv.Alias,
		"sql":    "UPDATE ADVANCE_BONUS_GRANT SET remark = 'MCP-REHEARSAL' WHERE emp_no = '0004'",
	})
	if res.IsError {
		t.Fatalf("rehearsal was refused: %s", resultText(t, res))
	}

	got := resultText(t, res)
	if !strings.Contains(got, "REHEARSAL") || !strings.Contains(got, "rolled back") {
		t.Errorf("response does not say it was a rehearsal:\n%s", got)
	}
	// The caveats are what stop "rolled back" being read as "safe to run".
	for _, want := range []string{"IDENTITY", "Locks were held"} {
		if !strings.Contains(got, want) {
			t.Errorf("rehearsal response omits the %q caveat:\n%s", want, got)
		}
	}

	var out struct {
		RowsAffected int64    `json:"rows_affected"`
		Committed    bool     `json:"committed"`
		Caveats      []string `json:"caveats"`
	}
	decodeStructured(t, res, &out)
	if out.Committed {
		t.Error("a rehearsal reported itself committed")
	}
	if len(out.Caveats) == 0 {
		t.Error("structured output carries no caveats")
	}
}

// TestCommitRequiresApproval covers the handshake an agent has to follow, and
// checks the response tells it what to do rather than just failing.
func TestCommitRequiresApproval(t *testing.T) {
	cs, svc := connectWrite(t)
	const stmt = "UPDATE ADVANCE_BONUS_GRANT SET remark = 'MCP-COMMIT-TEST' WHERE emp_no = '0004'"

	res := call(t, cs, "mssql_execute", map[string]any{
		"target": testenv.Alias, "sql": stmt, "commit": true,
	})
	if !res.IsError {
		t.Fatal("a commit without approval was allowed through")
	}
	got := resultText(t, res)
	for _, want := range []string{"APPROVAL REQUIRED", "nothing has run", "hrm-sql-mcp approve"} {
		if !strings.Contains(got, want) {
			t.Errorf("response missing %q:\n%s", want, got)
		}
	}

	// Nothing may have been written.
	check := call(t, cs, "mssql_query", map[string]any{
		"target": testenv.Alias,
		"sql":    "SELECT COUNT(*) AS n FROM ADVANCE_BONUS_GRANT WHERE remark = 'MCP-COMMIT-TEST'",
	})
	if strings.Contains(resultText(t, check), "\n1\n") {
		t.Error("the refused commit wrote anyway")
	}

	// And the request really is pending for a person to act on.
	pending, err := svc.Approvals().Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		t.Fatal("no approval request was raised")
	}
	var mine bool
	for _, p := range pending {
		if p.Statement == stmt {
			mine = true
			if p.Summary == "" {
				t.Error("the request carries no classification for the approver to read")
			}
		}
	}
	if !mine {
		t.Error("the pending request does not carry the statement it is for")
	}
}

// TestDeployRefusesUnwritableTarget: writability is a policy fact, and the
// refusal has to say so rather than looking like a permissions accident.

func TestDeployRefusesUnwritableTarget(t *testing.T) {
	cs, _ := connectWrite(t)
	res := call(t, cs, "mssql_execute", map[string]any{
		"target": testenv.AliasOther, // policy marks this one read-only
		"sql":    "UPDATE ADVANCE_BONUS_GRANT SET remark = 'x' WHERE 1 = 0",
	})
	if !res.IsError {
		t.Fatal("a write against a read-only target was allowed")
	}
	if got := resultText(t, res); !strings.Contains(got, "not writable") {
		t.Errorf("refusal does not explain why:\n%s", got)
	}
}
