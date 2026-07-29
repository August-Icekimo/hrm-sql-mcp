package schemadict

import (
	"sort"
	"strings"
)

// MatchField says which part of the dictionary matched.
type MatchField string

const (
	MatchTableName   MatchField = "table_name"
	MatchColumnName  MatchField = "column_name"
	MatchDescription MatchField = "description"
	MatchSystem      MatchField = "system"
	MatchRemark      MatchField = "remark"
)

// Match is one search hit.
type Match struct {
	Table  string     `json:"table"`
	System string     `json:"system,omitempty"`
	Field  MatchField `json:"matched"`
	// Column is set for column-level matches.
	Column *Column `json:"column,omitempty"`
	Line   int     `json:"line,omitempty"`
	// Score orders results; higher is a better match. See rank.
	Score int `json:"-"`
}

// SearchOptions tunes a search.
type SearchOptions struct {
	// Limit caps the returned matches. Zero means 50.
	Limit int
	// TablesOnly restricts matching to table names and systems.
	TablesOnly bool
}

// Search finds tables and columns matching a query.
//
// Matching is a case-insensitive substring test across identifiers and Chinese
// descriptions alike, because the caller does not know which of the two it has.
// An agent asked about 特休 has a Chinese phrase; an agent reading a stack
// trace has leave_days. Both have to land somewhere.
func (d *Dict) Search(query string, opt SearchOptions) []Match {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if opt.Limit <= 0 {
		opt.Limit = 50
	}

	var out []Match
	for i := range d.Tables {
		t := &d.Tables[i]

		if s := rank(t.Name, q); s > 0 {
			out = append(out, Match{Table: t.Name, System: t.System, Field: MatchTableName, Line: t.Line, Score: s + 20})
		} else if s := rank(t.System, q); s > 0 {
			out = append(out, Match{Table: t.Name, System: t.System, Field: MatchSystem, Line: t.Line, Score: s})
		}
		if opt.TablesOnly {
			continue
		}

		for j := range t.Columns {
			c := &t.Columns[j]
			field, score := bestColumnMatch(c, q)
			if score == 0 {
				continue
			}
			col := *c
			out = append(out, Match{
				Table: t.Name, System: t.System, Field: field,
				Column: &col, Line: t.Line, Score: score,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return columnName(out[i]) < columnName(out[j])
	})
	if len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out
}

func bestColumnMatch(c *Column, q string) (MatchField, int) {
	// Ordered by how much a hit tells the caller. A column whose name matches
	// is almost certainly the one being looked for; a match in 備註 is a hint.
	if s := rank(c.Name, q); s > 0 {
		return MatchColumnName, s + 10
	}
	if s := rank(c.Description, q); s > 0 {
		return MatchDescription, s + 5
	}
	if s := rank(c.Remark, q); s > 0 {
		return MatchRemark, s
	}
	return "", 0
}

// rank scores a substring hit: exact beats prefix beats contains.
//
// Without it, searching emp_no returns every column containing "emp" in file
// order, and the exact match is wherever it happens to fall.
func rank(field, q string) int {
	if field == "" {
		return 0
	}
	f := strings.ToLower(field)
	switch {
	case f == q:
		return 100
	case strings.HasPrefix(f, q):
		return 50
	case strings.Contains(f, q):
		return 25
	}
	return 0
}

func columnName(m Match) string {
	if m.Column == nil {
		return ""
	}
	return m.Column.Name
}
