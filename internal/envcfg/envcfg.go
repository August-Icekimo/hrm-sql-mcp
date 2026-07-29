// Package envcfg resolves settings from process environment variables and
// dotenv files, and remembers where each value came from.
//
// The provenance is not a nicety. Once host, database and credentials can each
// arrive from three places, "what did this run actually connect to, and why"
// stops being answerable by reading one file. Every lookup here records its
// source so `hrm-sql-mcp targets` can print it, which is the only thing that
// keeps a layered configuration debuggable.
//
// Precedence, highest first:
//
//  1. the process environment
//  2. dotenv files, in the order given
//  3. whatever the caller had already (the policy file)
//
// The process environment wins because that is what a CI runner, a container
// and an MCP client registration can all set, and because a value someone
// exported deliberately for one command should beat a file they forgot about.
package envcfg

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Origin says where a value came from, for display.
type Origin struct {
	// Kind is "env", "file" or "policy".
	Kind string
	// Detail is the file path for Kind=="file", and empty otherwise.
	Detail string
}

func (o Origin) String() string {
	if o.Kind == "file" {
		return "file:" + o.Detail
	}
	return o.Kind
}

// OriginPolicy marks a value the policy file supplied and nothing overrode.
var OriginPolicy = Origin{Kind: "policy"}

// Set is a resolved view over the process environment and any dotenv files.
type Set struct {
	files []fileVals
}

type fileVals struct {
	path string
	kv   map[string]string
}

// Load reads the given dotenv files, in precedence order.
//
// A path that does not exist is skipped rather than refused when optional is
// true. That is what lets the same invocation work on a developer machine
// (credentials in a file) and in CI (credentials in the environment), which is
// the entire reason this package exists.
func Load(paths []string, optional bool) (*Set, error) {
	s := &Set{}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		p = ExpandHome(p)
		kv, err := readFile(p)
		if err != nil {
			if os.IsNotExist(err) && optional {
				continue
			}
			return nil, err
		}
		s.files = append(s.files, fileVals{path: p, kv: kv})
	}
	return s, nil
}

// Lookup returns a value and where it came from.
func (s *Set) Lookup(key string) (string, Origin, bool) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, Origin{Kind: "env"}, true
	}
	for _, f := range s.files {
		if v, ok := f.kv[key]; ok && v != "" {
			return v, Origin{Kind: "file", Detail: f.path}, true
		}
	}
	return "", Origin{}, false
}

// readFile parses KEY=VALUE lines, enforcing 0600 on any file that holds a
// secret.
//
// The permission requirement is triggered by content rather than by filename.
// A file of database names is ordinary configuration and 0644 is fine; the
// moment a password appears in it, it is a credentials file whatever it is
// called, and a credentials file the whole machine can read is not one. Keying
// the rule off the path instead would be defeated by naming.
func readFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	kv := make(map[string]string)
	hasSecret := false
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, ln)
		}
		k = strings.TrimSpace(k)
		if IsSecretKey(k) {
			hasSecret = true
		}
		kv[k] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if hasSecret {
		if perm := info.Mode().Perm(); perm != 0o600 {
			return nil, fmt.Errorf(
				"%s holds a password but has mode %04o, must be 0600 (run: chmod 600 %s)",
				path, perm, path)
		}
	}
	return kv, nil
}

// IsSecretKey reports whether a key's value must never be displayed or stored
// in a world-readable file.
func IsSecretKey(k string) bool {
	return strings.HasSuffix(strings.ToUpper(strings.TrimSpace(k)), "_PASSWORD")
}

// ExpandHome turns a leading ~/ into the user's home directory.
func ExpandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// Paths returns the dotenv files that were actually read, for display.
func (s *Set) Paths() []string {
	out := make([]string, 0, len(s.files))
	for _, f := range s.files {
		out = append(out, f.path)
	}
	return out
}
