// Package sqlname reads the object names people and scripts hand to this tool.
//
// # Why splitting on the last dot is not enough
//
// HRM's procedures are called sp_X, so "everything after the last dot is the
// name" happened to work there. Other systems put dots inside the name itself:
// EIP's procedures are named like [EIP.humanly.BasicSetup.PersonalRestDayBase_1],
// one bracketed identifier. Splitting that on the last dot asks the server for
// a procedure called PersonalRestDayBase_1 in a schema called
// EIP.humanly.BasicSetup, and the honest-looking answer is "not found" — the
// tool reporting a procedure missing while it sits on the server.
//
// So brackets (and double quotes) are honoured as T-SQL does: a dot inside a
// delimited identifier is part of the name, and ]] is an escaped ].
//
// # Undelimited dotted input is ambiguous, and Candidates says so
//
// People do not type brackets. EIP.Dashboard_1 could be procedure Dashboard_1
// in schema EIP, or the single identifier [EIP.Dashboard_1] — EIP has 42 names
// of the second shape. Neither reading can be preferred from the text alone,
// so Candidates returns both and the caller lets the catalog decide. Picking
// one silently would reintroduce the false "not found" above for whichever
// shape lost.
package sqlname

import (
	"fmt"
	"strings"
)

// Name is a procedure or object name with an optional schema.
type Name struct {
	// Schema is empty when the input did not name one; the server then applies
	// its usual resolution (default schema, then dbo).
	Schema string
	// Object is the identifier with delimiters removed and escapes undone.
	Object string
}

// Quoted renders the name for OBJECT_ID and similar functions, which parse
// their argument as a multi-part name and would otherwise split on every dot.
func (n Name) Quoted() string {
	if n.Schema == "" {
		return Quote(n.Object)
	}
	return Quote(n.Schema) + "." + Quote(n.Object)
}

// Quote delimits one identifier, doubling any ] as T-SQL requires.
func Quote(id string) string {
	return "[" + strings.ReplaceAll(id, "]", "]]") + "]"
}

// part is one dot-separated piece of the input.
type part struct {
	text      string
	delimited bool
}

// split breaks s on dots that are outside [..] and "..".
func split(s string) ([]part, error) {
	var parts []part
	var cur strings.Builder
	delimited := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '[', '"':
			closer := byte(']')
			if c == '"' {
				closer = '"'
			}
			j := i + 1
			for {
				if j >= len(s) {
					return nil, fmt.Errorf("unterminated %c in name %q", c, s)
				}
				if s[j] == closer {
					if j+1 < len(s) && s[j+1] == closer { // escaped closer
						cur.WriteByte(closer)
						j += 2
						continue
					}
					break
				}
				cur.WriteByte(s[j])
				j++
			}
			delimited = true
			i = j
		case '.':
			parts = append(parts, part{strings.TrimSpace(cur.String()), delimited})
			cur.Reset()
			delimited = false
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, part{strings.TrimSpace(cur.String()), delimited})
	for _, p := range parts {
		if p.text == "" {
			return nil, fmt.Errorf("empty identifier in name %q", s)
		}
	}
	return parts, nil
}

// Candidates returns every reading of s, most conventional first.
//
//   - sp_x, [sp_x], [EIP.a.b_1]        → one candidate, no schema
//   - dbo.sp_x, [dbo].[EIP.a.b_1]      → one candidate, schema-qualified
//   - EIP.Dashboard_1 (undelimited)    → schema EIP + Dashboard_1, then the
//     whole text as one identifier
//   - EIP.humanly.X_1 (undelimited)    → the whole text as one identifier
//
// Three or more parts with any delimiter would mean database.schema.object,
// which this tool never resolves across databases, so it is an error rather
// than a guess.
func Candidates(s string) ([]Name, error) {
	s = strings.TrimSpace(s)
	parts, err := split(s)
	if err != nil {
		return nil, err
	}
	anyDelimited := false
	for _, p := range parts {
		anyDelimited = anyDelimited || p.delimited
	}

	switch {
	case len(parts) == 1:
		return []Name{{Object: parts[0].text}}, nil
	case anyDelimited && len(parts) == 2:
		return []Name{{Schema: parts[0].text, Object: parts[1].text}}, nil
	case anyDelimited:
		return nil, fmt.Errorf("name %q has %d parts; only object or schema.object is supported (bracket a name that contains dots)", s, len(parts))
	case len(parts) == 2:
		return []Name{
			{Schema: parts[0].text, Object: parts[1].text},
			{Object: s},
		}, nil
	default:
		// a.b.c undelimited: no schema in practice is called "a.b", and
		// cross-database names are out of scope, so the only useful reading
		// is the single identifier.
		return []Name{{Object: s}}, nil
	}
}

// Keys lower-cases the object part of every candidate, for matching a
// user-supplied filter against catalog and file keys: EIP.Dashboard_1 then
// matches whichever reading the catalog actually holds. An unparsable name
// falls back to its raw text, which simply matches nothing rather than
// aborting the whole comparison.
func Keys(s string) []string {
	c, err := Candidates(s)
	if err != nil || len(c) == 0 {
		return []string{strings.ToLower(strings.TrimSpace(s))}
	}
	out := make([]string, 0, len(c))
	for _, n := range c {
		out = append(out, strings.ToLower(n.Object))
	}
	return out
}
