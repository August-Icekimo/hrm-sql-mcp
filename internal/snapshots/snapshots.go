// Package snapshots keeps the previous version of anything this tool
// overwrites.
//
// Deploying a procedure replaces text nobody may have a copy of. The scripts
// in the repository are known to disagree with the server — that is what the
// audit exists to measure — so "we can always redeploy the file" is exactly
// the assumption this project has already disproved. Measured on HRM: 151
// procedures live on the server with no source file at all.
//
// So the definition is read and saved before it is replaced, and the audit
// record names the file. It is not a backup system; it is the one copy that
// would otherwise not exist.
package snapshots

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Store is a directory of saved pre-change definitions.
type Store struct{ dir string }

// Open prepares the snapshot directory, 0700: these files are procedure
// bodies from a payroll database.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("snapshot directory is empty")
	}
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the directory.
func (s *Store) Dir() string { return s.dir }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Save writes a pre-change definition and returns its path.
//
// An empty definition is still saved, with a marker: "this object did not
// exist before" is a fact worth being able to prove later, and an absent file
// cannot distinguish that from "nobody took a snapshot".
func (s *Store) Save(alias, database, name, definition string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	base := fmt.Sprintf("%s-%s-%s-%s.sql",
		stamp, safe(alias), safe(database), safe(name))
	path := filepath.Join(s.dir, base)

	body := definition
	if strings.TrimSpace(body) == "" {
		body = fmt.Sprintf("-- %s did not exist in %s/%s at %s.\n"+
			"-- This file records that absence; there was nothing to save.\n",
			name, alias, database, stamp)
	}
	header := fmt.Sprintf("-- hrm-sql-mcp snapshot taken %s\n-- target: %s  database: %s  object: %s\n"+
		"-- This is the definition as it stood BEFORE the deployment that followed.\n\n",
		stamp, alias, database, name)

	if err := os.WriteFile(path, []byte(header+body), 0o600); err != nil {
		return "", fmt.Errorf("write snapshot: %w", err)
	}
	return path, nil
}

func safe(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if s == "" {
		return "unnamed"
	}
	return s
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
