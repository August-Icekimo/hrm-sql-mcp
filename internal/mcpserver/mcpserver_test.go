package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/codex-k8s/hrm-sql-mcp/internal/mcpserver"
	"github.com/codex-k8s/hrm-sql-mcp/internal/service"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

// connect starts the server and a client over the SDK's in-memory transport.
//
// Driving a real client rather than calling the handlers directly is what
// makes this a test of the MCP surface: schema inference, argument decoding
// and result encoding all run, and those are the parts a direct call skips.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	testenv.RequireIntegration(t)

	svc, err := service.New()
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	srv := mcpserver.New(svc)
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
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestToolsAreRegisteredAndReadOnly also proves the schemas are inferable:
// mcp.AddTool panics on a type it cannot derive a schema from, so a broken
// input struct fails here rather than when a client first connects.
func TestToolsAreRegisteredAndReadOnly(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// An allowlist, not a count. A tool appearing here that nobody added on
	// purpose is the thing to catch — this is the test that fails the day a
	// write tool is registered into the read-only set by accident.
	want := map[string]bool{
		"mssql_targets": false, "mssql_query": false, "mssql_explain": false,
		"mssql_deps": false, "mssql_schema_search": false, "mssql_sp_list": false,
		"mssql_sp_get": false, "mssql_sp_diff": false, "mssql_sp_audit": false,
	}
	for _, tool := range res.Tools {
		if _, expected := want[tool.Name]; !expected {
			t.Errorf("unexpected tool %q — a write tool must never appear in this set", tool.Name)
			continue
		}
		want[tool.Name] = true

		if tool.Description == "" {
			t.Errorf("%s has no description; an agent has nothing else to go on", tool.Name)
		}
		// Phase 1 is read-only, and the annotation is how a client knows that
		// without parsing the name.
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q was not registered", name)
		}
	}
}

func TestTargetsTool(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "mssql_targets", nil)
	if res.IsError {
		t.Fatalf("mssql_targets failed: %s", resultText(t, res))
	}
	if got := resultText(t, res); !strings.Contains(got, testenv.Alias) {
		t.Errorf("target %q missing from response:\n%s", testenv.Alias, got)
	}

	var out struct {
		Profile string `json:"profile"`
		Targets []struct {
			Alias   string `json:"alias"`
			Guard   string `json:"guard"`
			Connect string `json:"connect"`
		} `json:"targets"`
	}
	decodeStructured(t, res, &out)
	if out.Profile != "local" {
		t.Errorf("profile = %q, want local", out.Profile)
	}
	if len(out.Targets) == 0 || out.Targets[0].Guard != "pass" || out.Targets[0].Connect != "ok" {
		t.Errorf("target status = %+v", out.Targets)
	}
}

func TestQueryTool(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "mssql_query", map[string]any{
		"target": testenv.Alias,
		"sql":    "SELECT 1 AS n, N'中文' AS s",
	})
	if res.IsError {
		t.Fatalf("mssql_query failed: %s", resultText(t, res))
	}

	got := resultText(t, res)
	// Every answer must say which database produced it. An agent has no other
	// way to know, and a result read as production when it came from local is
	// the mistake this line prevents.
	if !strings.Contains(got, "hrm") || !strings.Contains(got, "hrm_mcp_ro") {
		t.Errorf("response does not identify the target and login:\n%s", got)
	}
	if !strings.Contains(got, "中文") {
		t.Errorf("response lost the Chinese text:\n%s", got)
	}

	var out struct {
		Target struct {
			Database string `json:"database"`
			Login    string `json:"login"`
			Mode     string `json:"mode"`
		} `json:"target"`
		Rows int `json:"rows"`
	}
	decodeStructured(t, res, &out)
	if out.Rows != 1 || out.Target.Mode != "readonly" || out.Target.Login == "" {
		t.Errorf("structured output = %+v", out)
	}
}

// TestQueryTruncationIsAnnounced guards the property an agent cannot verify
// for itself: a capped result must not read as a complete one.
func TestQueryTruncationIsAnnounced(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "mssql_query", map[string]any{
		"target":   testenv.Alias,
		"sql":      "SELECT TOP 100 name FROM sys.all_objects ORDER BY name",
		"max_rows": 5,
	})
	if res.IsError {
		t.Fatalf("mssql_query failed: %s", resultText(t, res))
	}
	got := resultText(t, res)
	if !strings.Contains(got, "TRUNCATED") {
		t.Errorf("truncation was not announced:\n%s", got)
	}

	var out struct {
		Rows      int  `json:"rows"`
		Truncated bool `json:"truncated"`
	}
	decodeStructured(t, res, &out)
	if out.Rows != 5 || !out.Truncated {
		t.Errorf("rows=%d truncated=%v, want 5 and true", out.Rows, out.Truncated)
	}
}

// TestWriteIsRefusedAsAToolError checks both halves of the contract: the
// database refuses, and the refusal reaches the model as a readable result
// rather than a protocol failure it cannot reason about.
func TestWriteIsRefusedAsAToolError(t *testing.T) {
	cs := connect(t)
	find := call(t, cs, "mssql_query", map[string]any{
		"target": testenv.Alias,
		"sql": "SELECT TOP 1 TABLE_SCHEMA + '.' + TABLE_NAME AS t FROM INFORMATION_SCHEMA.TABLES " +
			"WHERE TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME",
	})
	if find.IsError {
		t.Fatalf("could not find a table: %s", resultText(t, find))
	}
	table := lastLine(resultText(t, find))
	if table == "" {
		t.Skip("no base tables in this database")
	}

	res := call(t, cs, "mssql_query", map[string]any{
		"target": testenv.Alias,
		"sql":    "DELETE FROM " + table + " WHERE 1 = 0",
	})
	if !res.IsError {
		t.Fatalf("DELETE on %s was not refused", table)
	}
	got := resultText(t, res)
	if !strings.Contains(got, "229") {
		t.Errorf("refusal does not name the SQL Server error number:\n%s", got)
	}
	if !strings.Contains(got, "Do not retry") {
		t.Errorf("refusal does not tell the model retrying is pointless:\n%s", got)
	}
}

func TestSPTools(t *testing.T) {
	cs := connect(t)

	list := call(t, cs, "mssql_sp_list", map[string]any{"target": testenv.Alias, "filter": "batch"})
	if list.IsError {
		t.Fatalf("mssql_sp_list failed: %s", resultText(t, list))
	}
	var listOut struct {
		Procs []struct {
			Name string `json:"name"`
		} `json:"procedures"`
		Total int `json:"total"`
	}
	decodeStructured(t, list, &listOut)
	if listOut.Total == 0 {
		t.Fatal("no procedures in the fixture database")
	}
	if len(listOut.Procs) == 0 {
		t.Fatal(`filter "batch" matched nothing`)
	}
	for _, p := range listOut.Procs {
		if !strings.Contains(strings.ToLower(p.Name), "batch") {
			t.Errorf("filter let %q through", p.Name)
		}
	}

	name := listOut.Procs[0].Name
	get := call(t, cs, "mssql_sp_get", map[string]any{"target": testenv.Alias, "name": name})
	if get.IsError {
		t.Fatalf("mssql_sp_get failed: %s", resultText(t, get))
	}
	var getOut struct {
		Definition string `json:"definition"`
	}
	decodeStructured(t, get, &getOut)
	bare := name[strings.LastIndex(name, ".")+1:]
	if !strings.Contains(strings.ToLower(getOut.Definition), strings.ToLower(bare)) {
		t.Errorf("definition of %s does not mention its own name", name)
	}

	missing := call(t, cs, "mssql_sp_get", map[string]any{
		"target": testenv.Alias, "name": "sp_does_not_exist_0000",
	})
	if !missing.IsError {
		t.Error("reading a missing procedure was not reported as an error")
	}
}

func TestSPDiffTool(t *testing.T) {
	testenv.RequireProjectRoot(t)
	cs := connect(t)
	res := call(t, cs, "mssql_sp_diff", map[string]any{"target": testenv.Alias})
	if res.IsError {
		t.Fatalf("mssql_sp_diff failed: %s", resultText(t, res))
	}
	var out struct {
		Counts map[string]int `json:"counts"`
	}
	decodeStructured(t, res, &out)
	if len(out.Counts) == 0 {
		t.Error("diff reported no counts at all")
	}
	// Identical rows are excluded from the listing by default but must still
	// be counted, or a caller cannot tell "nothing matched" from "nothing was
	// compared".
	if _, ok := out.Counts["identical"]; !ok {
		t.Errorf("identical procedures are not counted: %v", out.Counts)
	}
}

func decodeStructured(t *testing.T, res *mcp.CallToolResult, into any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode structured content: %v\n%s", err, raw)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func TestExplainTool(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "mssql_explain", map[string]any{
		"target": testenv.Alias,
		"sql":    "SELECT o.name FROM sys.objects o JOIN sys.columns c ON c.object_id = o.object_id",
	})
	if res.IsError {
		t.Fatalf("mssql_explain failed: %s", resultText(t, res))
	}

	got := resultText(t, res)
	// The model must not read a plan as a result set. Saying it outright is
	// cheaper than hoping the tool description was read.
	if !strings.Contains(got, "nothing was executed") {
		t.Errorf("response does not say the statement was not executed:\n%s", got)
	}

	var out struct {
		TotalCost  float64 `json:"total_cost"`
		XMLBytes   int     `json:"xml_bytes"`
		Statements []struct {
			EstimatedRows float64 `json:"estimated_rows"`
		} `json:"statements"`
		XML string `json:"xml"`
	}
	decodeStructured(t, res, &out)
	if out.TotalCost <= 0 || len(out.Statements) == 0 {
		t.Errorf("plan summary looks empty: %+v", out)
	}
	// The raw XML is thousands of bytes and was not asked for.
	if out.XML != "" {
		t.Errorf("plan XML was returned without include_xml")
	}
	if out.XMLBytes == 0 {
		t.Error("xml_bytes should report the plan size even when the XML is omitted")
	}
}

func TestExplainToolReportsCompileErrors(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "mssql_explain", map[string]any{
		"target": testenv.Alias,
		"sql":    "SELECT * FROM no_such_table_0000",
	})
	if !res.IsError {
		t.Fatalf("explaining a bad statement was not reported as an error:\n%s", resultText(t, res))
	}
}

func TestDepsTool(t *testing.T) {
	cs := connect(t)

	// Find a table something references, so the assertion is about the tool
	// rather than about which fixture happens to be restored.
	find := call(t, cs, "mssql_query", map[string]any{
		"target": testenv.Alias,
		"sql": `SELECT TOP 1 s.name + '.' + t.name AS n FROM sys.tables t
		        JOIN sys.schemas s ON s.schema_id = t.schema_id
		        JOIN sys.sql_expression_dependencies d ON d.referenced_id = t.object_id
		        GROUP BY s.name, t.name ORDER BY COUNT(*) DESC`,
	})
	if find.IsError {
		t.Fatalf("could not find a referenced table: %s", resultText(t, find))
	}
	table := lastLine(resultText(t, find))
	if table == "" || !strings.Contains(table, ".") {
		t.Skip("no referenced table in this database")
	}

	res := call(t, cs, "mssql_deps", map[string]any{
		"target": testenv.Alias, "name": table, "direction": "used_by",
	})
	if res.IsError {
		t.Fatalf("mssql_deps failed: %s", resultText(t, res))
	}

	var out struct {
		Dependencies []struct {
			Name      string `json:"name"`
			Direction string `json:"direction"`
			Exists    bool   `json:"exists"`
		} `json:"dependencies"`
		Caveat string `json:"caveat"`
	}
	decodeStructured(t, res, &out)
	if len(out.Dependencies) == 0 {
		t.Fatalf("%s is referenced but deps returned nothing", table)
	}
	for _, d := range out.Dependencies {
		if d.Direction != "used_by" {
			t.Errorf("direction filter ignored: %+v", d)
		}
	}
	// The caveat travels in the payload, not only the tool description: an
	// empty dependency list is the answer most likely to be over-read.
	if !strings.Contains(out.Caveat, "EXEC(@sql)") {
		t.Errorf("caveat does not mention dynamic SQL: %q", out.Caveat)
	}
	if !strings.Contains(resultText(t, res), "sp audit") && !strings.Contains(resultText(t, res), "mssql_sp_audit") {
		t.Error("response does not point at the audit tool for application call sites")
	}
}

func TestSchemaSearchTool(t *testing.T) {
	testenv.RequireProjectRoot(t)
	cs := connect(t)

	res := call(t, cs, "mssql_schema_search", map[string]any{"query": "emp_no", "limit": 5})
	if res.IsError {
		t.Fatalf("mssql_schema_search failed: %s", resultText(t, res))
	}

	var out struct {
		Source  string `json:"source"`
		Note    string `json:"note"`
		Matches []struct {
			Table   string `json:"table"`
			Matched string `json:"matched"`
			Column  *struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"column"`
		} `json:"matches"`
	}
	decodeStructured(t, res, &out)
	if len(out.Matches) == 0 {
		t.Fatal("emp_no matched nothing in the dictionary")
	}
	if out.Source == "" {
		t.Error("the dictionary path is not reported, so a caller cannot tell where the answer came from")
	}
	// The answer is from a file, not the server. A caller that mistakes it for
	// live schema will trust a column that may no longer exist.
	if !strings.Contains(out.Note, "not the live database") {
		t.Errorf("note does not say the source is a document: %q", out.Note)
	}
	if out.Matches[0].Column == nil || out.Matches[0].Column.Name != "emp_no" {
		t.Errorf("top match is not the exact column: %+v", out.Matches[0])
	}
}

// TestSchemaSearchByChineseDescription is the reason the dictionary is worth
// having: the catalog cannot answer this at all.
func TestSchemaSearchByChineseDescription(t *testing.T) {
	testenv.RequireProjectRoot(t)
	cs := connect(t)

	res := call(t, cs, "mssql_schema_search", map[string]any{"query": "事假", "limit": 10})
	if res.IsError {
		t.Fatalf("mssql_schema_search failed: %s", resultText(t, res))
	}
	var out struct {
		Matches []struct {
			Matched string `json:"matched"`
			Column  *struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"column"`
		} `json:"matches"`
	}
	decodeStructured(t, res, &out)
	if len(out.Matches) == 0 {
		t.Fatal("事假 matched nothing; the Chinese descriptions are not being searched")
	}
	for _, m := range out.Matches {
		if m.Column != nil && m.Matched == "description" &&
			!strings.Contains(m.Column.Description, "事假") {
			t.Errorf("match claims a description hit that is not there: %+v", m.Column)
		}
	}
}
