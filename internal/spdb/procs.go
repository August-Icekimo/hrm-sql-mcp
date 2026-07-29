package spdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Proc is one procedure as the server holds it.
type Proc struct {
	// Schema is the owning schema, usually dbo.
	Schema string
	// Name is the procedure name with the server's casing preserved.
	Name string
	// Type is sys.procedures.type_desc, e.g. SQL_STORED_PROCEDURE.
	Type string
	// Definition is the text of the last CREATE or ALTER. Empty when
	// Encrypted is true — see that field.
	Definition string
	// Encrypted marks a procedure created WITH ENCRYPTION. Its definition is
	// not readable, so it can be listed but never compared. Reporting this
	// separately matters: an encrypted procedure treated as "empty definition"
	// would show up as a difference against every file on disk.
	Encrypted bool
	// Created and Modified come from the catalog.
	Created, Modified time.Time
}

// Key is the lower-cased bare name, which is how files and Java call sites
// refer to procedures. SQL Server identifiers are case-insensitive under
// HRM's collation, so matching on anything else would invent differences.
func (p Proc) Key() string { return strings.ToLower(p.Name) }

// Qualified renders schema.name for display.
func (p Proc) Qualified() string { return p.Schema + "." + p.Name }

// listQuery reads the catalog. is_ms_shipped filters out the procedures SQL
// Server installs itself, which are not anybody's to audit.
//
// The LEFT JOIN is deliberate: an encrypted procedure has a row in
// sys.procedures and none in sys.sql_modules, and an INNER JOIN would make it
// vanish from the inventory entirely rather than appear as unreadable.
const listQuery = `
SELECT s.name, p.name, p.type_desc, p.create_date, p.modify_date, %s
FROM sys.procedures AS p
JOIN sys.schemas AS s ON s.schema_id = p.schema_id
LEFT JOIN sys.sql_modules AS m ON m.object_id = p.object_id
WHERE p.is_ms_shipped = 0
ORDER BY s.name, p.name`

// List returns every procedure without its definition.
func List(ctx context.Context, db *sql.DB) ([]Proc, error) {
	return query(ctx, db, fmt.Sprintf(listQuery, "CAST(NULL AS nvarchar(max)), CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END"))
}

// LoadAll returns every procedure with its definition.
//
// One round trip rather than one per procedure: HRM has a few hundred, and a
// per-procedure loop would take long enough that people would stop running the
// audit — an audit nobody runs is the same as no audit.
func LoadAll(ctx context.Context, db *sql.DB) ([]Proc, error) {
	return query(ctx, db, fmt.Sprintf(listQuery, "m.definition, CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END"))
}

// Get returns one procedure by bare or schema-qualified name.
func Get(ctx context.Context, db *sql.DB, name string) (Proc, error) {
	schema, bare := splitName(name)
	q := `
SELECT s.name, p.name, p.type_desc, p.create_date, p.modify_date,
       m.definition, CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END
FROM sys.procedures AS p
JOIN sys.schemas AS s ON s.schema_id = p.schema_id
LEFT JOIN sys.sql_modules AS m ON m.object_id = p.object_id
WHERE p.name = @name AND (@schema = '' OR s.name = @schema)`
	got, err := query(ctx, db, q, sql.Named("name", bare), sql.Named("schema", schema))
	if err != nil {
		return Proc{}, err
	}
	switch len(got) {
	case 0:
		return Proc{}, fmt.Errorf("procedure %q not found", name)
	case 1:
		return got[0], nil
	default:
		return Proc{}, fmt.Errorf("procedure %q is ambiguous across %d schemas; qualify it", name, len(got))
	}
}

func query(ctx context.Context, db *sql.DB, q string, args ...any) ([]Proc, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read sys.procedures: %w", err)
	}
	defer rows.Close()

	var out []Proc
	for rows.Next() {
		var p Proc
		var def sql.NullString
		var enc bool
		if err := rows.Scan(&p.Schema, &p.Name, &p.Type, &p.Created, &p.Modified, &def, &enc); err != nil {
			return nil, fmt.Errorf("scan sys.procedures: %w", err)
		}
		p.Definition = def.String
		p.Encrypted = enc
		out = append(out, p)
	}
	return out, rows.Err()
}

// splitName separates an optional schema prefix and strips [brackets].
func splitName(name string) (schema, bare string) {
	unbracket := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "[")
		return strings.TrimSuffix(s, "]")
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		return unbracket(name[:i]), unbracket(name[i+1:])
	}
	return "", unbracket(name)
}

// Index maps Key to procedure and reports names defined in more than one
// schema.
//
// Duplicates are returned rather than resolved. The rest of the audit matches
// on the bare name because that is all a Java call site gives us, so a name
// living in two schemas makes every claim about it ambiguous — the honest move
// is to say so, not to pick one.
func Index(procs []Proc) (map[string]Proc, []string) {
	byKey := make(map[string]Proc, len(procs))
	var dups []string
	for _, p := range procs {
		if prev, seen := byKey[p.Key()]; seen {
			dups = append(dups, prev.Qualified()+" / "+p.Qualified())
			continue
		}
		byKey[p.Key()] = p
	}
	return byKey, dups
}
