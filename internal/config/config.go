package config

import (
	"strings"

	"github.com/caarlos0/env/v11"

	"github.com/codex-k8s/hrm-sql-mcp/internal/envcfg"
	"github.com/codex-k8s/hrm-sql-mcp/internal/target"
)

// Config holds environment-driven settings.
type Config struct {
	// PolicyPath points at the consuming project's policy file.
	PolicyPath string `env:"HRM_SQL_MCP_POLICY" envDefault:"mcp/hrm-sql.yaml"`
	// Profile must be stated explicitly; there is no default on purpose.
	Profile string `env:"HRM_SQL_MCP_PROFILE"`
	// CredentialsPath is the 0600 file holding the two logins.
	//
	// No longer the only way to supply them: the same keys are read from the
	// process environment, which is how a CI runner or a container gets them
	// in without writing a file first. The file keeps its 0600 requirement;
	// the environment cannot have one, which is the trade being made.
	CredentialsPath string `env:"HRM_SQL_MCP_CREDENTIALS" envDefault:"~/.config/hrm-sql-mcp/credentials.env"`
	// EnvFiles are extra dotenv files, comma separated, lowest precedence
	// first. Values here are overridden by the process environment.
	//
	// Separate from CredentialsPath so a repository can ship a checked-in file
	// of non-secret settings (database names, hosts) without that file being
	// held to the 0600 rule that secrets require.
	EnvFiles string `env:"HRM_SQL_MCP_ENV_FILE"`
	// ProjectRoot is what the policy's relative paths (sp_dir, java_src_dir)
	// resolve against. It defaults to the working directory because that is
	// where an MCP client spawns the server — the same assumption that lets
	// .mcp.json say `--policy mcp/hrm-sql.yaml` without a machine-specific
	// absolute path.
	ProjectRoot string `env:"HRM_SQL_MCP_PROJECT_ROOT" envDefault:"."`
	// LogLevel sets the slog level.
	LogLevel string `env:"HRM_SQL_MCP_LOG_LEVEL" envDefault:"info"`
	// Actor names who is driving the tool, and lands in every audit record.
	// The MCP client registrations set it so that one shared log can still
	// answer "which agent ran this", which the PID alone cannot once the
	// process has exited.
	Actor string `env:"HRM_SQL_MCP_ACTOR" envDefault:"cli"`
}

// Load parses the environment.
func Load() (Config, error) {
	c, err := env.ParseAs[Config]()
	if err != nil {
		return c, err
	}
	c.CredentialsPath = envcfg.ExpandHome(c.CredentialsPath)
	return c, nil
}

// SourcePaths lists the dotenv files to read, lowest precedence first.
//
// The credentials file comes last so that, among files, the one holding
// secrets wins. The process environment still beats all of them.
func (c Config) SourcePaths() []string {
	var out []string
	for _, p := range strings.Split(c.EnvFiles, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return append(out, c.CredentialsPath)
}

// LoadSource reads the dotenv files named by this config.
//
// Missing files are skipped rather than refused. That is what lets one
// invocation work on a laptop, where credentials live in a 0600 file, and in
// CI, where they arrive as environment variables and no file exists. A
// credential that is genuinely absent still fails — at the point of use, with
// an error naming the credential key it looked for.
func (c Config) LoadSource() (*envcfg.Set, error) {
	return envcfg.Load(c.SourcePaths(), true)
}

// Store resolves credentials from the layered source.
type Store struct{ src *envcfg.Set }

// NewStore wraps a resolved source for credential lookups.
//
// Expected keys, upper-cased credential key:
//
//	HRM_SQL_<KEY>_RO_USER / _RO_PASSWORD
//	HRM_SQL_<KEY>_RW_USER / _RW_PASSWORD
//
// The same names work as process environment variables and as dotenv lines,
// so moving a deployment from a laptop to CI is a change of where they are
// set, not of what they are called.
func NewStore(src *envcfg.Set) *Store { return &Store{src: src} }

// CredentialKeyBase builds the shared prefix for one credential key and mode.
func CredentialKeyBase(key string, mode target.AccessMode) string {
	suffix := "RO"
	if mode == target.ReadWrite {
		suffix = "RW"
	}
	return "HRM_SQL_" + strings.ToUpper(strings.TrimSpace(key)) + "_" + suffix
}

// Lookup returns the user and password for a credential key and access mode.
//
// The key comes from the policy target's credential_key, so several targets
// can share one entry. A missing entry returns ok=false, which callers must
// treat as "do not connect" — never as "connect without credentials".
func (s *Store) Lookup(key string, mode target.AccessMode) (string, string, bool) {
	if s == nil || s.src == nil {
		return "", "", false
	}
	base := CredentialKeyBase(key, mode)
	user, _, uok := s.src.Lookup(base + "_USER")
	pass, _, pok := s.src.Lookup(base + "_PASSWORD")
	if !uok || !pok || user == "" || pass == "" {
		return "", "", false
	}
	return user, pass, true
}

// Origin reports where a credential was found, for the targets listing. It
// never returns the value itself.
func (s *Store) Origin(key string, mode target.AccessMode) (string, bool) {
	if s == nil || s.src == nil {
		return "", false
	}
	base := CredentialKeyBase(key, mode)
	if _, o, ok := s.src.Lookup(base + "_PASSWORD"); ok {
		return o.String(), true
	}
	return "", false
}

// Credentials adapts the store to the target package.
func (s *Store) Credentials() target.Credentials {
	return target.Credentials{Lookup: s.Lookup}
}
