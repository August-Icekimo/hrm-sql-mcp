package spdb_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

// dottedFixture creates procedures whose names contain dots, the way EIP
// names all of its procedures, inside a transaction that is always rolled
// back: the snapshot is shared, and a test that leaves objects on it changes
// every inventory run after it.
//
// Two shapes are created on purpose. [dbo].[hrm_mcp_it.Dotted_1] has a dot
// inside one identifier; [dbo].[hrm_mcp_it.Caller_1] calls it, so Deps has an
// edge to find in each direction.
func dottedFixture(t *testing.T) (*sql.Tx, context.Context) {
	t.Helper()
	db := testenv.OpenWritable(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	for _, stmt := range []string{
		"CREATE PROCEDURE [dbo].[hrm_mcp_it.Dotted_1] AS SELECT 1 AS one",
		"CREATE PROCEDURE [dbo].[hrm_mcp_it.Caller_1] AS EXEC [dbo].[hrm_mcp_it.Dotted_1]",
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(err.Error(), "permission") {
				t.Skipf("writable login cannot create procedures here: %v", err)
			}
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return tx, ctx
}

// TestGetDottedName is the regression this package was changed for: every
// form a person or a VB call site would write must reach the procedure. The
// old last-dot split asked for Dotted_1 in a schema called hrm_mcp_it and
// reported "not found".
func TestGetDottedName(t *testing.T) {
	tx, ctx := dottedFixture(t)

	for _, name := range []string{
		"[hrm_mcp_it.Dotted_1]",
		"[dbo].[hrm_mcp_it.Dotted_1]",
		"dbo.[hrm_mcp_it.Dotted_1]",
		"hrm_mcp_it.Dotted_1", // undelimited: the whole-name reading must be tried
	} {
		got, err := spdb.Get(ctx, tx, name)
		if err != nil {
			t.Errorf("Get(%q): %v", name, err)
			continue
		}
		if got.Name != "hrm_mcp_it.Dotted_1" || got.Schema != "dbo" {
			t.Errorf("Get(%q) = %s.%s", name, got.Schema, got.Name)
		}
		if !strings.Contains(got.Definition, "SELECT 1 AS one") {
			t.Errorf("Get(%q) definition = %q", name, got.Definition)
		}
	}
}

func TestDepsDottedName(t *testing.T) {
	tx, ctx := dottedFixture(t)

	uses, err := spdb.Deps(ctx, tx, "[hrm_mcp_it.Caller_1]", spdb.Uses)
	if err != nil {
		t.Fatalf("Deps uses: %v", err)
	}
	if !hasDep(uses, "hrm_mcp_it.Dotted_1") {
		t.Errorf("Caller_1 uses = %+v, want an edge to hrm_mcp_it.Dotted_1", uses)
	}

	usedBy, err := spdb.Deps(ctx, tx, "hrm_mcp_it.Dotted_1", spdb.UsedBy)
	if err != nil {
		t.Fatalf("Deps used_by: %v", err)
	}
	if !hasDep(usedBy, "hrm_mcp_it.Caller_1") {
		t.Errorf("Dotted_1 used_by = %+v, want an edge from hrm_mcp_it.Caller_1", usedBy)
	}
}

func hasDep(deps []spdb.Dependency, suffix string) bool {
	for _, d := range deps {
		if strings.HasSuffix(strings.ToLower(d.Name), strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}
