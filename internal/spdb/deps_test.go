package spdb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/codex-k8s/hrm-sql-mcp/internal/spdb"
	"github.com/codex-k8s/hrm-sql-mcp/internal/testenv"
)

func TestDepsUses(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	procs, err := spdb.LoadAll(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// Find any procedure with dependencies rather than naming one: this
	// fixture is a restored snapshot, and hard-coding sp_SRJ0300 would make
	// the test fail for a reason that has nothing to do with the code.
	var found string
	var deps []spdb.Dependency
	for _, p := range procs {
		got, err := spdb.Deps(ctx, db, p.Qualified(), spdb.Uses)
		if err != nil {
			t.Fatalf("Deps(%s): %v", p.Qualified(), err)
		}
		if len(got) > 0 {
			found, deps = p.Qualified(), got
			break
		}
	}
	if found == "" {
		t.Skip("no procedure in this database references anything")
	}
	t.Logf("%s references %d objects", found, len(deps))

	for _, d := range deps {
		if d.Name == "" {
			t.Errorf("dependency with no name: %+v", d)
		}
		if d.Direction != spdb.Uses {
			t.Errorf("direction = %q, want uses", d.Direction)
		}
		// Exists must be false only when the type is unknown too; a row that
		// claims an object is missing while naming its type would mean the
		// LEFT JOIN and the flag disagree.
		if !d.Exists && d.Type != "" {
			t.Errorf("%s is marked missing but has type %q", d.Name, d.Type)
		}
	}
}

func TestDepsUsedBy(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	// A table is the useful direction to test: procedures reference tables,
	// so used_by should find them.
	rows, err := db.QueryContext(ctx,
		`SELECT TOP 1 s.name + '.' + t.name FROM sys.tables t
		 JOIN sys.schemas s ON s.schema_id = t.schema_id
		 JOIN sys.sql_expression_dependencies d ON d.referenced_id = t.object_id
		 GROUP BY s.name, t.name ORDER BY COUNT(*) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var table string
	if rows.Next() {
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	if table == "" {
		t.Skip("no table is referenced by any module in this database")
	}

	deps, err := spdb.Deps(ctx, db, table, spdb.UsedBy)
	if err != nil {
		t.Fatalf("Deps(%s, used_by): %v", table, err)
	}
	if len(deps) == 0 {
		t.Fatalf("%s is referenced according to the catalog, but used_by found nothing", table)
	}
	t.Logf("%s is referenced by %d modules", table, len(deps))
	for _, d := range deps {
		if d.Direction != spdb.UsedBy {
			t.Errorf("direction = %q, want used_by", d.Direction)
		}
		if !strings.Contains(d.Name, ".") {
			t.Errorf("used_by name %q is not schema-qualified", d.Name)
		}
	}
}

// TestDepsBothCombines checks that Both is exactly the two directions, not a
// third query with its own behaviour.
func TestDepsBothCombines(t *testing.T) {
	db, _ := testenv.Open(t)
	ctx := context.Background()

	procs, err := spdb.LoadAll(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) == 0 {
		t.Skip("no procedures")
	}
	name := procs[0].Qualified()

	uses, err := spdb.Deps(ctx, db, name, spdb.Uses)
	if err != nil {
		t.Fatal(err)
	}
	usedBy, err := spdb.Deps(ctx, db, name, spdb.UsedBy)
	if err != nil {
		t.Fatal(err)
	}
	both, err := spdb.Deps(ctx, db, name, spdb.Both)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != len(uses)+len(usedBy) {
		t.Errorf("both returned %d, uses %d + used_by %d", len(both), len(uses), len(usedBy))
	}
}

func TestDepsRejectsUnknownDirection(t *testing.T) {
	db, _ := testenv.Open(t)
	if _, err := spdb.Deps(context.Background(), db, "dbo.whatever", spdb.Direction("sideways")); err == nil {
		t.Error("an unknown direction was accepted")
	}
}

// TestDepsOnMissingObject: OBJECT_ID returns NULL for a name that does not
// exist, which makes the queries return nothing. That must be an empty result,
// not an error — "nothing references this" and "this does not exist" are both
// legitimate answers and the caller can tell them apart with mssql_sp_get.
func TestDepsOnMissingObject(t *testing.T) {
	db, _ := testenv.Open(t)
	deps, err := spdb.Deps(context.Background(), db, "dbo.sp_does_not_exist_0000", spdb.Both)
	if err != nil {
		t.Fatalf("Deps on a missing object errored: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("got %d dependencies for a missing object", len(deps))
	}
}
