package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"

	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// Config holds environment-driven settings.
type Config struct {
	// PolicyPath points at the consuming project's policy file.
	PolicyPath string `env:"HRM_SQL_MCP_POLICY" envDefault:"mcp/hrm-sql.yaml"`
	// Profile must be stated explicitly; there is no default on purpose.
	Profile string `env:"HRM_SQL_MCP_PROFILE"`
	// CredentialsPath is the 0600 file holding the two logins.
	CredentialsPath string `env:"HRM_SQL_MCP_CREDENTIALS" envDefault:"~/.config/hrm-sql-mcp/credentials.env"`
	// LogLevel sets the slog level.
	LogLevel string `env:"HRM_SQL_MCP_LOG_LEVEL" envDefault:"info"`
}

// Load parses the environment.
func Load() (Config, error) {
	c, err := env.ParseAs[Config]()
	if err != nil {
		return c, err
	}
	c.CredentialsPath = expandHome(c.CredentialsPath)
	return c, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// Store holds credentials keyed by alias and mode.
type Store struct{ kv map[string]string }

// LoadCredentials reads the credentials file.
//
// The file must be mode 0600. This is enforced rather than merely documented:
// a credentials file that anyone on the machine can read is not a credentials
// file, and the failure is silent unless something checks. The deploy script
// takes the same stance with chmod 700 on setenv.sh.
//
// Expected keys, upper-cased alias:
//
//	HRM_SQL_<ALIAS>_RO_USER / _RO_PASSWORD
//	HRM_SQL_<ALIAS>_RW_USER / _RW_PASSWORD
func LoadCredentials(path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("credentials file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, fmt.Errorf(
			"credentials file %s has mode %04o, must be 0600 (run: chmod 600 %s)",
			path, perm, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open credentials %s: %w", path, err)
	}
	defer f.Close()

	kv := make(map[string]string)
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, ln)
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read credentials %s: %w", path, err)
	}
	return &Store{kv: kv}, nil
}

// Lookup returns the user and password for an alias and access mode.
//
// A missing entry returns ok=false, which callers must treat as "do not
// connect" — never as "connect without credentials".
func (s *Store) Lookup(alias string, mode target.AccessMode) (string, string, bool) {
	suffix := "RO"
	if mode == target.ReadWrite {
		suffix = "RW"
	}
	base := "HRM_SQL_" + strings.ToUpper(alias) + "_" + suffix
	user, uok := s.kv[base+"_USER"]
	pass, pok := s.kv[base+"_PASSWORD"]
	if !uok || !pok || user == "" || pass == "" {
		return "", "", false
	}
	return user, pass, true
}

// Credentials adapts the store to the target package.
func (s *Store) Credentials() target.Credentials {
	return target.Credentials{Lookup: s.Lookup}
}
