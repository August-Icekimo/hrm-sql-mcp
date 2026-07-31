package spfile

import (
	"regexp"
	"strings"
)

var leadingCreateOrAlter = regexp.MustCompile(
	`(?i)^([ \t]*)(CREATE[ \t]+OR[ \t]+ALTER|CREATE|ALTER)([ \t]+PROC)`)

// Normalize prepares a definition for comparison.
//
// It does exactly four things, and the restraint is the design:
//
//  1. CRLF and lone CR become LF. Files written on Windows versus what the
//     server returns differ here for reasons nobody cares about.
//  2. Trailing whitespace on each line is removed. Editors add it silently.
//  3. Trailing blank lines are removed.
//  4. A leading CREATE — or CREATE OR ALTER — is rewritten to ALTER.
//     sys.sql_modules stores the text of the most recent CREATE *or* ALTER, so
//     a procedure that has ever been altered reads as ALTER on the server
//     while the file on disk still says CREATE. Treating them as different
//     would flag almost every procedure. CREATE OR ALTER is stored verbatim by
//     the server, so folding all three spellings to one keeps a file and a
//     server that were deployed via different statements comparable.
//
// Deliberately NOT done: case folding, re-indenting, collapsing runs of
// spaces, sorting anything. Those are real differences between the file and
// the server. Smoothing them over would turn this tool into a placebo that
// reports "identical" for procedures that are not.
func Normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	out := strings.Join(lines, "\n")

	return leadingCreateOrAlter.ReplaceAllString(out, "${1}ALTER${3}")
}

// Equal reports whether a file definition and a server definition match after
// normalisation.
func Equal(fileDef, serverDef string) bool {
	return Normalize(fileDef) == Normalize(serverDef)
}
