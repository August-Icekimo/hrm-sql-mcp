package javascan

import "strings"

// Literal is one string literal with the line it starts on.
type Literal struct {
	Text string
	Line int
}

// Literals extracts every string literal from Java source, skipping comments.
//
// This is a lexer, not a parser: it only needs to know where literals and
// comments begin and end. Java's grammar makes that decidable from characters
// alone, with one catch — a backslash inside a literal escapes the next
// character, so the naive "read to the next quote" rule ends a literal early on
// "\"" and then treats the following code as string content, which cascades
// through the rest of the file.
func Literals(src string) []Literal {
	var out []Literal
	line := 1
	for i := 0; i < len(src); {
		switch {
		case src[i] == '\n':
			line++
			i++

		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}

		case strings.HasPrefix(src[i:], "/*"):
			i += 2
			for i < len(src) && !strings.HasPrefix(src[i:], "*/") {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2

		case strings.HasPrefix(src[i:], `"""`):
			// Text block. The closing delimiter is the next unescaped """.
			start, startLine := i+3, line
			i += 3
			for i < len(src) && !strings.HasPrefix(src[i:], `"""`) {
				if src[i] == '\\' {
					i++
				} else if src[i] == '\n' {
					line++
				}
				i++
			}
			out = append(out, Literal{Text: src[start:min(i, len(src))], Line: startLine})
			i += 3

		case src[i] == '"':
			start, startLine := i+1, line
			i++
			for i < len(src) && src[i] != '"' {
				switch src[i] {
				case '\\':
					i++ // skip the escaped character, including \" and \\
				case '\n':
					// Unterminated: Java forbids a raw newline in a literal, so
					// this is either broken source or a lexing mistake on our
					// part. Either way, ending the literal here contains the
					// damage to one line instead of losing the rest of the file.
					line++
				}
				i++
			}
			out = append(out, Literal{Text: src[start:min(i, len(src))], Line: startLine})
			i++

		case src[i] == '\'':
			// Char literal. Not a source of procedure names, but it must be
			// consumed so that '"' does not open a phantom string.
			i++
			for i < len(src) && src[i] != '\'' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
			i++

		default:
			i++
		}
	}
	return out
}
