package javascan

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codex-k8s/hrm-sql-mcp/internal/spfile"
)

// DefaultCallPattern matches a procedure name in call position: after EXEC,
// EXECUTE, or the JDBC {call ...} escape, with an optional schema prefix.
// Submatch 1 is the name.
//
// Requiring the keyword is what keeps the ghost list honest. Measured against
// HRM: a plain sp_[a-z0-9_]+ search over string literals reports sp_amount as
// a called procedure, because WDC0900Action builds "... ,0) sp_amount " — a
// column alias that happens to follow the naming convention. One fabricated
// incident in a list of five is enough for a reader to stop trusting the list.
var DefaultCallPattern = regexp.MustCompile(
	`(?i)\b(?:exec|execute|call)\b[\s{(]*(?:\[?[a-z0-9_]+\]?\s*\.\s*)?\[?(sp_[a-z0-9_]+)`)

// DefaultMentionPattern matches the naming convention anywhere in a literal.
//
// Mentions exist because the call pattern cannot see a name that is assembled
// at runtime — "exec " + name, with the name sitting in its own literal or a
// constant. Too weak to raise an alarm on, but strong enough to stop us
// declaring a procedure unused.
var DefaultMentionPattern = regexp.MustCompile(`(?i)\bsp_[a-z0-9_]+`)

// Kind separates evidence strong enough to act on from evidence that only
// rules something out.
type Kind string

const (
	// KindCall is a name in call position: EXEC, EXECUTE or {call ...}.
	KindCall Kind = "call"
	// KindMention is a name appearing in a literal in any other position.
	KindMention Kind = "mention"
)

// Site is one occurrence of a procedure name in a string literal.
type Site struct {
	// File is relative to the scanned root, so reports are portable.
	File string
	Line int
	// Name is the lower-cased procedure name.
	Name string
	// Kind says how strong this evidence is.
	Kind Kind
	// Literal is the literal it was found in, trimmed for display. Seeing the
	// surrounding text is what lets a reviewer judge a match in one glance.
	Literal string
}

// Result is the outcome of scanning a source tree.
type Result struct {
	// Sites is every match, ordered by file then line.
	Sites []Site
	// ByName groups sites by lower-cased procedure name, calls before mentions.
	ByName map[string][]Site
	// Files is how many source files were read.
	Files int
	// Failures records files that could not be decoded, keyed by path.
	// They are reported rather than skipped: a file we could not read might be
	// the only caller of a procedure, and treating it as "no calls" would turn
	// an unknown into a confident wrong answer.
	Failures map[string]error
}

// Options tunes a scan.
type Options struct {
	// CallPattern defaults to DefaultCallPattern. Submatch 1 is the name.
	CallPattern *regexp.Regexp
	// MentionPattern defaults to DefaultMentionPattern.
	MentionPattern *regexp.Regexp
	// Exts are the file extensions to read, lower-cased with the dot.
	// Defaults to .java.
	Exts []string
	// SkipDirs are directory names pruned anywhere in the tree.
	SkipDirs []string
	// PathPrefix is prepended to every reported path. Set it to the scanned
	// directory's own name so that sites read as project-relative paths a
	// reader can paste into an editor.
	PathPrefix string
}

func (o Options) withDefaults() Options {
	if o.CallPattern == nil {
		o.CallPattern = DefaultCallPattern
	}
	if o.MentionPattern == nil {
		o.MentionPattern = DefaultMentionPattern
	}
	if len(o.Exts) == 0 {
		o.Exts = []string{".java"}
	}
	if o.SkipDirs == nil {
		o.SkipDirs = []string{".git", "build", "bin", "target", "node_modules"}
	}
	return o
}

// Scan walks root and collects procedure names from string literals.
func Scan(root string, opts Options) (*Result, error) {
	opts = opts.withDefaults()
	skip := make(map[string]struct{}, len(opts.SkipDirs))
	for _, d := range opts.SkipDirs {
		skip[d] = struct{}{}
	}

	res := &Result{ByName: map[string][]Site{}, Failures: map[string]error{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, drop := skip[d.Name()]; drop {
				return fs.SkipDir
			}
			return nil
		}
		if !hasExt(path, opts.Exts) {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		if opts.PathPrefix != "" {
			rel = filepath.Join(opts.PathPrefix, rel)
		}
		res.Files++

		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			res.Failures[rel] = rerr
			return nil
		}
		// Same decoder as the SQL scripts: this tree is mostly UTF-8, but a
		// decade-old Java file with Big5 comments would otherwise abort the
		// walk or silently lose its literals.
		text, _, derr := spfile.Decode(raw)
		if derr != nil {
			res.Failures[rel] = derr
			return nil
		}
		res.Sites = append(res.Sites, sitesIn(rel, text, opts)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(res.Sites, func(i, j int) bool {
		if res.Sites[i].File != res.Sites[j].File {
			return res.Sites[i].File < res.Sites[j].File
		}
		if res.Sites[i].Line != res.Sites[j].Line {
			return res.Sites[i].Line < res.Sites[j].Line
		}
		return res.Sites[i].Name < res.Sites[j].Name
	})
	// Calls first within each name, so a report that shows only the first site
	// shows the strongest one.
	for _, s := range res.Sites {
		if s.Kind == KindCall {
			res.ByName[s.Name] = append(res.ByName[s.Name], s)
		}
	}
	for _, s := range res.Sites {
		if s.Kind == KindMention {
			res.ByName[s.Name] = append(res.ByName[s.Name], s)
		}
	}
	return res, nil
}

// Calls returns the call-position sites for a name.
func (r *Result) Calls(name string) []Site {
	var out []Site
	for _, s := range r.ByName[name] {
		if s.Kind == KindCall {
			out = append(out, s)
		}
	}
	return out
}

// sitesIn finds matches within one file's literals.
//
// Call matches are taken first so that a name in call position is never also
// counted as a mention of itself — one occurrence is one site, and its kind is
// the strongest thing it qualifies as.
func sitesIn(file, src string, opts Options) []Site {
	var out []Site
	for _, lit := range Literals(src) {
		// One literal naming the same procedure twice is one call site, not
		// two. Counting it twice would overstate how used a procedure is.
		seen := map[string]struct{}{}
		add := func(name string, kind Kind) {
			name = strings.ToLower(name)
			if _, dup := seen[name]; dup {
				return
			}
			seen[name] = struct{}{}
			out = append(out, Site{
				File:    file,
				Line:    lit.Line,
				Name:    name,
				Kind:    kind,
				Literal: trimLiteral(lit.Text),
			})
		}
		for _, m := range opts.CallPattern.FindAllStringSubmatch(lit.Text, -1) {
			add(m[1], KindCall)
		}
		for _, m := range opts.MentionPattern.FindAllString(lit.Text, -1) {
			add(m, KindMention)
		}
	}
	return out
}

// trimLiteral collapses a literal to one readable line for the report.
func trimLiteral(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 120
	// Counted in runes, not bytes: these literals contain Chinese, and cutting
	// mid-rune would put a replacement character into the report.
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

func hasExt(path string, exts []string) bool {
	e := strings.ToLower(filepath.Ext(path))
	for _, want := range exts {
		if e == want {
			return true
		}
	}
	return false
}
