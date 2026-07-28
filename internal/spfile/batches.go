package spfile

import (
	"strings"
)

// StripLiterals replaces the contents of string literals, quoted identifiers
// and comments with spaces of the same length, leaving everything else intact.
//
// Equal length is the point: offsets into the stripped text still line up with
// the original, so a caller can report a line number from a match found here.
//
// This exists because every naive scan of T-SQL is wrong. A batch separator,
// a keyword, or a procedure name can all appear inside a string or a comment,
// and HRM's scripts are full of Chinese prose in both. Handled:
//
//	'...'     with '' as the escaped quote
//	"..."     quoted identifier
//	[...]     bracketed identifier
//	-- ...    to end of line
//	/* ... */ nested, which T-SQL genuinely supports
func StripLiterals(s string) string { return strip(s, true) }

// StripComments blanks comments and string literals but leaves quoted and
// bracketed identifiers intact.
//
// The distinction matters: [dbo].[sp_Foo] is a procedure *name*, not a
// literal. Blanking it before matching a CREATE PROCEDURE header would erase
// the very thing being looked for — which it did, until a test caught it.
func StripComments(s string) string { return strip(s, false) }

func strip(s string, blankIdentifiers bool) string {
	out := []byte(s)
	blank := func(i, j int) {
		for k := i; k < j && k < len(out); k++ {
			if out[k] != '\n' { // keep newlines so line numbers survive
				out[k] = ' '
			}
		}
	}

	for i := 0; i < len(s); {
		switch {
		case s[i] == '\'':
			j := i + 1
			for j < len(s) {
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' { // '' escape
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			blank(i+1, j-1)
			i = j

		case s[i] == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				j++
			}
			if blankIdentifiers {
				blank(i+1, j)
			}
			i = min(j+1, len(s))

		case s[i] == '[':
			j := i + 1
			for j < len(s) && s[j] != ']' {
				j++
			}
			if blankIdentifiers {
				blank(i+1, j)
			}
			i = min(j+1, len(s))

		case strings.HasPrefix(s[i:], "--"):
			j := i
			for j < len(s) && s[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j

		case strings.HasPrefix(s[i:], "/*"):
			depth, j := 1, i+2
			for j < len(s) && depth > 0 {
				switch {
				case strings.HasPrefix(s[j:], "/*"):
					depth++
					j += 2
				case strings.HasPrefix(s[j:], "*/"):
					depth--
					j += 2
				default:
					j++
				}
			}
			blank(i, j)
			i = j

		default:
			i++
		}
	}
	return string(out)
}

// Batch is one GO-separated section of a script.
type Batch struct {
	// Text is the batch content, without the trailing GO line.
	Text string
	// StartLine is the 1-based line of Text within the original file.
	StartLine int
}

// SplitBatches divides a script on GO separators.
//
// GO is not T-SQL — the server rejects it. It is a client-side instruction to
// sqlcmd/SSMS meaning "send what I have so far", and these scripts are full of
// it because they are SSMS output. To compare a file against
// sys.sql_modules we need the one batch holding CREATE/ALTER PROCEDURE, so the
// separators have to be understood rather than passed through.
//
// A separator is a line whose only content is GO, optionally followed by a
// repeat count. Detection runs against the literal-stripped text so a GO
// inside a comment or string is not mistaken for one.
func SplitBatches(script string) []Batch {
	stripped := StripLiterals(script)
	origLines := strings.Split(script, "\n")
	strpLines := strings.Split(stripped, "\n")

	var (
		out     []Batch
		cur     []string
		curFrom = 1
	)
	flush := func(endLine int) {
		text := strings.Join(cur, "\n")
		if strings.TrimSpace(text) != "" {
			out = append(out, Batch{Text: text, StartLine: curFrom})
		}
		cur = nil
		curFrom = endLine + 1
	}

	for i, raw := range origLines {
		probe := ""
		if i < len(strpLines) {
			probe = strpLines[i]
		}
		if isGoSeparator(probe) {
			flush(i + 1)
			continue
		}
		cur = append(cur, raw)
	}
	flush(len(origLines))
	return out
}

// isGoSeparator reports whether a line is a batch separator.
func isGoSeparator(line string) bool {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) == 0 || !strings.EqualFold(f[0], "GO") {
		return false
	}
	if len(f) == 1 {
		return true
	}
	if len(f) > 2 {
		return false
	}
	// "GO 5" — a repeat count is still a separator.
	for _, r := range f[1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
