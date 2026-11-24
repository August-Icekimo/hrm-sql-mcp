package spaudit

import (
	"fmt"
	"os"
	"sort"
	"strings"

	yaml "github.com/yaml/go-yaml"
)

// Notes are human findings attached to individual procedures.
//
// # Why the report needs them at all
//
// Everything else in a report is derived: the file, the catalog and the Java
// scan are re-read on every run, so the document can be regenerated from
// scratch and nothing is lost. That property is worth keeping — it is why the
// file says "do not edit by hand".
//
// But it means a conclusion that took real investigation has nowhere to live.
// The case that forced this: sp_WDC0100 sits in `identical` because a Java
// call site exists at WDC0100Action.java:672, and by the audit's model that is
// a healthy procedure. It is not. The screen was taken off the menu, nobody
// holds permission to run it, and the table it maintains is now written by a
// different system entirely over a linked server. None of that is visible to
// any of the three sources, and all of it had to be found by asking people and
// reading a production menu table.
//
// Without somewhere to record it, the next reader repeats the investigation —
// or, worse, reads `identical` as "fine".
//
// # A note annotates, it never suppresses
//
// Notes deliberately cannot change a row's status, hide it, or remove it from
// a count. A mechanism that could silence findings would be used to silence
// findings, and an inventory whose clean sections cannot be trusted is worth
// less than no inventory. The strongest thing a note can say is "we looked
// into this one, here is what we found" — the row stays exactly where the
// evidence puts it.
//
// # Format
//
// A YAML mapping of procedure name to note, e.g.
//
//	sp_WDC0100: |
//	  已退役：MENU 已下線、無人有執行權限。
//	  LEAVE_SUM_DATA 改由差勤系統跨 linked server 寫入。
//
// Names are matched case-insensitively, because the report normalises them to
// lower case while the scripts and everyone's memory use mixed case.
type Notes map[string]string

// LoadNotes reads a notes file. A missing file is not an error: the notes are
// optional, and a project that has never written one should not see failures.
//
// Anything else — unreadable, malformed, wrong shape — is an error. The
// alternative is a typo silently costing every note in the file, which is the
// failure this whole mechanism exists to prevent.
func LoadNotes(path string) (Notes, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read notes %s: %w", path, err)
	}

	var parsed map[string]string
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse notes %s: %w", path, err)
	}

	out := make(Notes, len(parsed))
	for k, v := range parsed {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Get returns the note for a procedure name, matched case-insensitively.
func (n Notes) Get(name string) string {
	if n == nil {
		return ""
	}
	return n[strings.ToLower(strings.TrimSpace(name))]
}

// Names returns the annotated procedure names, sorted.
func (n Notes) Names() []string {
	out := make([]string, 0, len(n))
	for k := range n {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
