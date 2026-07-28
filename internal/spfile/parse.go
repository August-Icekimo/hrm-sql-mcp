package spfile

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// procHeader matches CREATE/ALTER PROCEDURE and captures the bare name.
// Run against literal-stripped text so a mention inside a comment is ignored.
var procHeader = regexp.MustCompile(
	`(?im)^[ \t]*(CREATE|ALTER)[ \t]+PROC(?:EDURE)?[ \t]+` +
		`(?:\[?[A-Za-z0-9_]+\]?[ \t]*\.[ \t]*)?` + // optional schema, e.g. dbo.
		`\[?([A-Za-z0-9_#@$]+)\]?`)

// Script is one parsed file from the stored-procedure directory.
type Script struct {
	// Path is the file path as given.
	Path string
	// Encoding is what Decode determined.
	Encoding string
	// Procs lists every procedure this file defines, lower-cased.
	Procs []string
	// Definition is the batch containing the first CREATE/ALTER PROCEDURE,
	// which is what sys.sql_modules stores for comparison.
	Definition string
	// DefinitionLine is where that batch starts in the file.
	DefinitionLine int
}

// ParseFile reads and parses one script.
func ParseFile(path string) (*Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, enc, err := Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s := ParseText(text)
	s.Path = path
	s.Encoding = enc
	return s, nil
}

// ParseText parses already-decoded script text.
func ParseText(text string) *Script {
	s := &Script{}
	seen := map[string]struct{}{}

	for _, b := range SplitBatches(text) {
		// StripComments, not StripLiterals: [dbo].[sp_Foo] is the name we are
		// looking for, so bracketed identifiers must survive.
		m := procHeader.FindStringSubmatch(StripComments(b.Text))
		if m == nil {
			continue
		}
		name := strings.ToLower(m[2])
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			s.Procs = append(s.Procs, name)
		}
		if s.Definition == "" {
			s.Definition = b.Text
			s.DefinitionLine = b.StartLine
		}
	}
	return s
}

// ScanDir parses every file in a stored-procedure directory.
//
// Files that fail to decode are returned as errors rather than skipped: a
// procedure silently missing from the inventory is precisely the failure this
// tool exists to catch, so the tool must not create it.
func ScanDir(dir string) (scripts []*Script, failures map[string]error, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	failures = map[string]error{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		low := strings.ToLower(name)
		if !strings.HasSuffix(low, ".sql") && !strings.HasSuffix(low, ".txt") {
			continue
		}
		sc, perr := ParseFile(dir + "/" + name)
		if perr != nil {
			failures[name] = perr
			continue
		}
		scripts = append(scripts, sc)
	}
	return scripts, failures, nil
}
