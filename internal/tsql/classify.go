package tsql

import (
	"regexp"
	"strings"
)

// Kind is a statement label.
type Kind string

const (
	KindSelect   Kind = "select"
	KindInsert   Kind = "insert"
	KindUpdate   Kind = "update"
	KindDelete   Kind = "delete"
	KindMerge    Kind = "merge"
	KindTruncate Kind = "truncate"
	KindDDL      Kind = "ddl"
	KindExec     Kind = "exec"
	KindDynamic  Kind = "dynamic"
	KindSet      Kind = "set"
	KindOther    Kind = "other"
	KindEmpty    Kind = "empty"
)

// Label describes a statement for a reader.
type Label struct {
	// Kinds are every kind found, in the order first seen. A batch can be
	// several things at once, and collapsing that to one word is how a DELETE
	// hides behind a SELECT.
	Kinds []Kind `json:"kinds"`
	// Dynamic reports that the batch builds SQL at run time, so what it
	// actually does cannot be known from the text.
	Dynamic bool `json:"dynamic"`
	// Objects are the table and procedure names mentioned, for the approval
	// prompt. Best effort: a name assembled at run time will not appear.
	Objects []string `json:"objects,omitempty"`
}

// Summary renders the label for a prompt or a log line.
func (l Label) Summary() string {
	if len(l.Kinds) == 0 {
		return string(KindEmpty)
	}
	parts := make([]string, len(l.Kinds))
	for i, k := range l.Kinds {
		parts[i] = string(k)
	}
	s := strings.Join(parts, "+")
	if l.Dynamic {
		s += " (builds SQL at run time — the text below is not the whole story)"
	}
	return s
}

// Writes reports whether any labelled kind changes data or schema.
//
// For display and for deciding how loudly to ask, never for deciding whether
// to allow. A batch this returns false for can still write, via dynamic SQL or
// a procedure call; that is why the read-only login exists.
func (l Label) Writes() bool {
	for _, k := range l.Kinds {
		switch k {
		case KindInsert, KindUpdate, KindDelete, KindMerge, KindTruncate, KindDDL:
			return true
		}
	}
	return false
}

var (
	// Anchored to a statement boundary so that a column named "update_date"
	// or a string containing "delete" does not get counted.
	kindPatterns = []struct {
		kind Kind
		re   *regexp.Regexp
	}{
		{KindSelect, regexp.MustCompile(`(?is)(^|[;\s(])select\s`)},
		{KindInsert, regexp.MustCompile(`(?is)(^|[;\s])insert\s`)},
		{KindUpdate, regexp.MustCompile(`(?is)(^|[;\s])update\s`)},
		{KindDelete, regexp.MustCompile(`(?is)(^|[;\s])delete\s`)},
		{KindMerge, regexp.MustCompile(`(?is)(^|[;\s])merge\s`)},
		{KindTruncate, regexp.MustCompile(`(?is)(^|[;\s])truncate\s+table\s`)},
		{KindDDL, regexp.MustCompile(`(?is)(^|[;\s])(create|alter|drop)\s+(proc|procedure|table|view|function|index|trigger|schema)\s`)},
		{KindExec, regexp.MustCompile(`(?is)(^|[;\s])(exec|execute)\s`)},
		// Anchored to the start of a statement, not to any whitespace: every
		// UPDATE contains the word SET, and labelling all of them "update+set"
		// puts noise in every single write label.
		{KindSet, regexp.MustCompile(`(?is)(^|[;])\s*set\s+\w+`)},
	}

	dynamicRe = regexp.MustCompile(`(?is)(sp_executesql|(^|[;\s])exec(ute)?\s*\()`)

	objectRe = regexp.MustCompile(`(?is)(?:from|join|into|update|delete\s+from|exec|execute)\s+(\[?[a-z0-9_]+\]?(?:\s*\.\s*\[?[a-z0-9_]+\]?){0,2})`)
)

// Classify labels a statement.
//
// Comments and string literals are stripped first. Without that, a batch whose
// only DELETE is inside a comment reads as a delete, and an approval prompt
// that cries wolf is one people learn to click through.
func Classify(statement string) Label {
	stripped := strip(statement)
	if strings.TrimSpace(stripped) == "" {
		return Label{}
	}

	var l Label
	seen := map[Kind]struct{}{}
	for _, p := range kindPatterns {
		if !p.re.MatchString(stripped) {
			continue
		}
		if _, dup := seen[p.kind]; dup {
			continue
		}
		seen[p.kind] = struct{}{}
		l.Kinds = append(l.Kinds, p.kind)
	}

	if dynamicRe.MatchString(stripped) {
		l.Dynamic = true
		l.Kinds = append(l.Kinds, KindDynamic)
	}
	if len(l.Kinds) == 0 {
		l.Kinds = []Kind{KindOther}
	}

	objSeen := map[string]struct{}{}
	for _, m := range objectRe.FindAllStringSubmatch(stripped, -1) {
		name := strings.ToLower(strings.Join(strings.Fields(m[1]), ""))
		name = strings.NewReplacer("[", "", "]", "").Replace(name)
		if name == "" {
			continue
		}
		if _, dup := objSeen[name]; dup {
			continue
		}
		objSeen[name] = struct{}{}
		l.Objects = append(l.Objects, name)
	}
	return l
}

// strip removes comments and string literal contents.
//
// Literal *contents* go, but the quotes stay, so that 'DELETE' inside a string
// stops being a delete while the statement's shape is preserved.
func strip(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case strings.HasPrefix(s[i:], "/*"):
			// T-SQL block comments nest, so track depth rather than scanning
			// for the first */.
			depth := 1
			i += 2
			for i < len(s) && depth > 0 {
				switch {
				case strings.HasPrefix(s[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(s[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			b.WriteByte(' ')
		case s[i] == '\'':
			b.WriteString("''")
			i++
			for i < len(s) {
				if s[i] == '\'' {
					// '' inside a literal is an escaped quote, not the end.
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case s[i] == '[':
			// Bracketed identifiers survive intact: they are names, and names
			// are what the object list is for.
			b.WriteByte(s[i])
			i++
			for i < len(s) && s[i] != ']' {
				b.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				b.WriteByte(']')
				i++
			}
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}
