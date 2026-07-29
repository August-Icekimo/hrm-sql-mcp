package spdb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Direction selects which way dependencies are followed.
type Direction string

const (
	// Uses lists what the named object references.
	Uses Direction = "uses"
	// UsedBy lists the SQL modules that reference the named object.
	UsedBy Direction = "used_by"
	// Both runs each of the above.
	Both Direction = "both"
)

// Dependency is one edge.
type Dependency struct {
	// Name is schema-qualified where the server knows the schema.
	Name string `json:"name"`
	// Type is the object's type_desc, empty when the reference points at
	// something that does not exist.
	Type      string    `json:"type,omitempty"`
	Direction Direction `json:"direction"`
	// Exists is false for a reference to a missing object — a script that
	// selects from a table nobody created. Worth its own field because it is
	// the one row in a dependency list that is a defect rather than a fact.
	Exists bool `json:"exists"`
	// CallerDependent marks a reference resolved at run time from the caller's
	// schema, so its target cannot be known statically.
	CallerDependent bool `json:"caller_dependent,omitempty"`
	// Ambiguous marks a reference the server could not resolve to one object.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// usesQuery reads what an object references.
//
// LEFT JOIN because sys.sql_expression_dependencies happily records a
// reference to an object that does not exist; that row is the interesting one.
const usesQuery = `
SELECT
    CASE WHEN d.referenced_schema_name IS NULL THEN d.referenced_entity_name
         ELSE d.referenced_schema_name + '.' + d.referenced_entity_name END,
    ISNULL(o.type_desc, ''),
    CASE WHEN o.object_id IS NULL THEN 0 ELSE 1 END,
    d.is_caller_dependent,
    d.is_ambiguous
FROM sys.sql_expression_dependencies AS d
LEFT JOIN sys.objects AS o ON o.object_id = d.referenced_id
WHERE d.referencing_id = OBJECT_ID(@name)`

// usedByQuery reads what references an object.
//
// The referenced_id IS NULL branch matters: a non-schema-bound reference is
// recorded by name, and the id is only filled in once the target resolves.
// Matching on the name as well is what keeps a procedure that calls a
// currently-missing table from disappearing from the answer.
const usedByQuery = `
SELECT DISTINCT
    s.name + '.' + o.name,
    o.type_desc,
    1,
    d.is_caller_dependent,
    d.is_ambiguous
FROM sys.sql_expression_dependencies AS d
JOIN sys.objects AS o ON o.object_id = d.referencing_id
JOIN sys.schemas AS s ON s.schema_id = o.schema_id
WHERE d.referenced_id = OBJECT_ID(@name)
   OR (d.referenced_id IS NULL AND d.referenced_entity_name = @bare)`

// Deps returns the dependency edges for an object.
//
// Two things it cannot see, both worth knowing before trusting an empty
// result:
//
//   - References built at run time. EXEC(@sql) is invisible to the catalog by
//     construction. Measured on HRM's snapshot this is 1 procedure of 252, so
//     the gap is small here, but it is not zero.
//   - Anything outside SQL. The application's own call sites are not in this
//     view at all; that is what the Java scan in package spaudit is for. An
//     "unused" verdict from here means unused *by other SQL modules*.
func Deps(ctx context.Context, db *sql.DB, name string, dir Direction) ([]Dependency, error) {
	schema, bare := splitName(name)
	qualified := bare
	if schema != "" {
		qualified = schema + "." + bare
	}

	var out []Dependency
	if dir == Uses || dir == Both {
		got, err := depQuery(ctx, db, usesQuery, Uses, sql.Named("name", qualified))
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	if dir == UsedBy || dir == Both {
		got, err := depQuery(ctx, db, usedByQuery, UsedBy,
			sql.Named("name", qualified), sql.Named("bare", bare))
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	if dir != Uses && dir != UsedBy && dir != Both {
		return nil, fmt.Errorf("unknown direction %q (want uses, used_by or both)", dir)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func depQuery(ctx context.Context, db *sql.DB, q string, dir Direction, args ...any) ([]Dependency, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read sys.sql_expression_dependencies: %w", err)
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	var out []Dependency
	for rows.Next() {
		var d Dependency
		var exists int
		if err := rows.Scan(&d.Name, &d.Type, &exists, &d.CallerDependent, &d.Ambiguous); err != nil {
			return nil, fmt.Errorf("scan dependency: %w", err)
		}
		d.Direction = dir
		d.Exists = exists == 1
		key := strings.ToLower(d.Name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}
	return out, rows.Err()
}
