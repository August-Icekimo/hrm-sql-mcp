package policy

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	yaml "github.com/yaml/go-yaml"

	"github.com/codex-k8s/hrm-sql-mcp/internal/constants"
)

// Policy is the parsed contents of the project's policy file.
type Policy struct {
	// Server describes how the MCP server presents itself.
	Server Server `yaml:"server"`
	// Profile must match HRM_SQL_MCP_PROFILE, see Validate.
	Profile string `yaml:"profile"`
	// Targets are the databases this project may reach.
	Targets []Target `yaml:"targets"`
	// Limits bound every query and statement.
	Limits Limits `yaml:"limits"`
	// Paths point at project files the tools read.
	Paths Paths `yaml:"paths"`
	// Audit configures where the append-only log is written.
	Audit Audit `yaml:"audit"`
}

// Server describes the MCP server identity.
type Server struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// Target is one reachable database.
type Target struct {
	// Alias is how tools refer to this target. Must be unique.
	Alias string `yaml:"alias"`
	// Host is a hostname or IP. Checked against the compile-time denylists.
	Host string `yaml:"host"`
	// Port defaults to 1433 when zero.
	Port int `yaml:"port"`
	// Database is the initial catalog.
	Database string `yaml:"database"`
	// AllowCIDRs is an allowlist every resolved IP must fall inside.
	// Empty means "reject everything" — fail closed, never fail open.
	AllowCIDRs []string `yaml:"allow_cidrs"`
	// Encrypt turns on TLS. Defaults to true.
	Encrypt *bool `yaml:"encrypt"`
	// TrustServerCertificate skips server identity verification.
	// Only reasonable inside a trusted network with self-signed certs;
	// it gives encryption without authentication, i.e. no MITM protection.
	TrustServerCertificate bool `yaml:"trust_server_certificate"`
	// AppName appears in sys.dm_exec_sessions.program_name so a DBA can tell
	// our connections apart from the application's.
	AppName string `yaml:"app_name"`
	// Writable allows the read-write login to be used against this target.
	// Read-only tools ignore it.
	Writable bool `yaml:"writable"`
}

// Limits bound query cost and blast radius.
type Limits struct {
	MaxRows        int           `yaml:"max_rows"`
	MaxBytes       int           `yaml:"max_bytes"`
	QueryTimeout   time.Duration `yaml:"query_timeout"`
	ExecuteTimeout time.Duration `yaml:"execute_timeout"`
	LockTimeout    time.Duration `yaml:"lock_timeout"`
}

// Paths point at files in the consuming project, relative to its root.
type Paths struct {
	SPDir      string `yaml:"sp_dir"`
	SchemaDict string `yaml:"schema_dict"`
	JavaSrcDir string `yaml:"java_src_dir"`
}

// Audit configures the append-only log.
type Audit struct {
	// File is the JSONL log. It has a default and cannot be set to empty:
	// there is no configuration that turns auditing off, because a policy
	// that could silently disable the record of what an agent did to this
	// database would defeat every other control in this file.
	File        string `yaml:"file"`
	SnapshotDir string `yaml:"snapshot_dir"`
}

// DefaultAuditFile is used when the policy does not name one.
const DefaultAuditFile = "~/.local/state/hrm-sql-mcp/audit.jsonl"

// Load reads and validates a policy file.
func Load(path string) (*Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy %s: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse policy %s: %w", path, err)
	}
	p.applyDefaults()
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid policy %s: %w", path, err)
	}
	return &p, nil
}

func (p *Policy) applyDefaults() {
	if p.Limits.MaxRows == 0 {
		p.Limits.MaxRows = 500
	}
	if p.Limits.MaxBytes == 0 {
		p.Limits.MaxBytes = 1 << 20
	}
	if p.Limits.QueryTimeout == 0 {
		p.Limits.QueryTimeout = 30 * time.Second
	}
	if p.Limits.ExecuteTimeout == 0 {
		p.Limits.ExecuteTimeout = 120 * time.Second
	}
	if p.Limits.LockTimeout == 0 {
		p.Limits.LockTimeout = 5 * time.Second
	}
	if strings.TrimSpace(p.Audit.File) == "" {
		p.Audit.File = DefaultAuditFile
	}
	for i := range p.Targets {
		if p.Targets[i].Port == 0 {
			p.Targets[i].Port = 1433
		}
		if p.Targets[i].Encrypt == nil {
			t := true
			p.Targets[i].Encrypt = &t
		}
	}
}

// Validate rejects any policy that could widen the blast radius.
//
// Every check here fails closed. A missing or empty field is treated as
// "deny", never as "no restriction" — the most common way an allowlist turns
// into a no-op is an empty list being read as "allow all".
func (p *Policy) Validate() error {
	if p.Profile == "" {
		return fmt.Errorf("profile is required and has no default")
	}
	if slices.Contains(constants.DeniedProfileNames, strings.ToLower(p.Profile)) {
		return fmt.Errorf("profile %q is permanently denied", p.Profile)
	}
	if !slices.Contains(constants.AllowedProfiles, p.Profile) {
		return fmt.Errorf("profile %q is not one of %v", p.Profile, constants.AllowedProfiles)
	}
	if len(p.Targets) == 0 {
		return fmt.Errorf("no targets declared")
	}

	seen := make(map[string]struct{}, len(p.Targets))
	for i, t := range p.Targets {
		switch {
		case t.Alias == "":
			return fmt.Errorf("targets[%d]: alias is required", i)
		case t.Host == "":
			return fmt.Errorf("target %q: host is required", t.Alias)
		case t.Database == "":
			return fmt.Errorf("target %q: database is required", t.Alias)
		case len(t.AllowCIDRs) == 0:
			// Fail closed: an absent allowlist must not mean "anything goes".
			return fmt.Errorf("target %q: allow_cidrs is required (empty means deny)", t.Alias)
		}
		if _, dup := seen[t.Alias]; dup {
			return fmt.Errorf("duplicate target alias %q", t.Alias)
		}
		seen[t.Alias] = struct{}{}
	}
	return nil
}

// TargetByAlias returns the declared target with the given alias.
func (p *Policy) TargetByAlias(alias string) (Target, bool) {
	for _, t := range p.Targets {
		if t.Alias == alias {
			return t, true
		}
	}
	return Target{}, false
}
