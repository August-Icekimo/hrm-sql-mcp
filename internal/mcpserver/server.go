package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
)

// New builds the MCP server with the read-only tool set.
//
// Only read-only tools exist here today. When write tools arrive they must be
// registered separately rather than added to this list, because the Gemini
// registration filters by tool name on the client side — a filter that is a
// convenience, not a boundary, and one that silently stops covering a tool
// nobody remembered to add to it.
func New(svc *service.Service) *mcp.Server {
	pol := svc.Policy()
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    orDefault(pol.Server.Name, "hrm-sql-mcp"),
		Version: orDefault(pol.Server.Version, "0.1.0"),
	}, nil)

	addTargets(srv, svc)
	addQuery(srv, svc)
	addSPList(srv, svc)
	addSPGet(srv, svc)
	addSPDiff(srv, svc)
	addSPAudit(srv, svc)
	return srv
}

// Run serves over stdio until the client disconnects.
func Run(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// readOnly marks a tool as making no modifications, which lets a client show
// the difference without having to interpret the name.
func readOnly(title string) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: &no,
		IdempotentHint:  true,
		// The reachable world is the targets the policy declares and the guard
		// approves, which is as closed as it gets.
		OpenWorldHint: &no,
	}
}

// targetEnvelope is embedded in every response so an answer always states
// where it came from.
type targetEnvelope struct {
	Alias    string `json:"alias"`
	Server   string `json:"server"`
	Database string `json:"database"`
	Login    string `json:"login"`
	Mode     string `json:"mode"`
}

func envelope(d map[string]string) targetEnvelope {
	return targetEnvelope{
		Alias:    d["alias"],
		Server:   d["server"],
		Database: d["database"],
		Login:    d["login"],
		Mode:     d["mode"],
	}
}

func (e targetEnvelope) header() string {
	return fmt.Sprintf("%s @ %s/%s as %s (%s)", e.Alias, e.Server, e.Database, e.Login, e.Mode)
}

// text builds the unstructured half of a tool result.
//
// Both halves are filled in: structured content for anything parsing the
// response, and text for the model reading it. Leaving the text to be
// auto-generated from the struct would hand an agent raw JSON to infer meaning
// from, when the parts that matter — truncation, which server, why a refusal
// happened — are exactly the parts a summary should state outright.
func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// fail renders an error as a tool result rather than a protocol error.
//
// MCP distinguishes the two, and the distinction matters here: a protocol
// error tells the client the call broke, while an IsError result tells the
// model its request was answered with a refusal it can read and act on. A
// permission denial is the second kind.
func fail(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
