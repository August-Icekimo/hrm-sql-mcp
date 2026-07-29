package policy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/codex-k8s/hrm-sql-mcp/internal/envcfg"
)

// Source supplies override values. Satisfied by *envcfg.Set.
type Source interface {
	Lookup(key string) (string, envcfg.Origin, bool)
}

// Override records one field that did not come from the policy file.
//
// Kept and displayed rather than applied silently. A configuration that can be
// changed from three places is only safe if a person can ask what it currently
// resolves to and get a complete answer; an override nobody can see is
// indistinguishable from the policy file lying.
type Override struct {
	Alias  string
	Field  string
	Key    string
	Value  string
	Origin envcfg.Origin
}

// String renders one override for the targets listing.
func (o Override) String() string {
	return fmt.Sprintf("%s.%s=%s (%s via %s)", o.Alias, o.Field, o.Value, o.Origin, o.Key)
}

// EnvPrefix is the prefix for every per-target override key.
const EnvPrefix = "HRM_SQL_TARGET_"

// ExtraTargetsKey names the variable that declares targets the policy file
// does not contain.
const ExtraTargetsKey = "HRM_SQL_MCP_EXTRA_TARGETS"

// envAlias converts an alias to the form used inside variable names.
func envAlias(alias string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(alias)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// key builds the variable name for one target field.
func key(alias, field string) string {
	return EnvPrefix + envAlias(alias) + "_" + field
}

// ApplyOverrides rewrites targets from the environment and dotenv files, and
// adds any target declared through ExtraTargetsKey.
//
// It runs before Validate, deliberately. Every guard-relevant field can be
// overridden here — host and allow_cidrs included — so the checks that keep a
// policy from widening the blast radius have to see the final values, not the
// ones the file happened to ship with. Applying overrides after validation
// would leave the validator auditing a configuration that never ran.
//
// What cannot be reached from here: the compile-time denylists in
// internal/constants. Setting allow_cidrs to 0.0.0.0/0 through the environment
// widens this policy's allowlist and changes nothing about production being
// unreachable, because the guard intersects the two and the denied side is not
// configuration.
func ApplyOverrides(p *Policy, src Source) ([]Override, error) {
	if src == nil {
		return nil, nil
	}

	var applied []Override
	note := func(alias, field, k, v string, o envcfg.Origin) {
		shown := v
		if envcfg.IsSecretKey(k) {
			shown = "(hidden)"
		}
		applied = append(applied, Override{Alias: alias, Field: field, Key: k, Value: shown, Origin: o})
	}

	extra, err := extraTargets(src)
	if err != nil {
		return nil, err
	}
	for _, t := range extra {
		if _, dup := p.TargetByAlias(t.Alias); dup {
			return nil, fmt.Errorf("%s declares %q, which the policy file already defines; "+
				"override its fields instead of redeclaring it", ExtraTargetsKey, t.Alias)
		}
		p.Targets = append(p.Targets, t)
		note(t.Alias, "declared", ExtraTargetsKey, t.Alias, envcfg.Origin{Kind: "env"})
	}

	for i := range p.Targets {
		t := &p.Targets[i]
		alias := t.Alias

		// get returns the value for one field, plus everything needed to
		// report it. The key is returned so error messages can name the exact
		// variable somebody has to go and fix.
		get := func(field string) (string, string, envcfg.Origin, bool) {
			k := key(alias, field)
			v, o, ok := src.Lookup(k)
			return v, k, o, ok
		}
		str := func(field string, dst *string) {
			if v, k, o, ok := get(field); ok {
				*dst = v
				note(alias, strings.ToLower(field), k, v, o)
			}
		}
		boolean := func(field string, dst *bool) error {
			v, k, o, ok := get(field)
			if !ok {
				return nil
			}
			b, err := parseBool(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*dst = b
			note(alias, strings.ToLower(field), k, v, o)
			return nil
		}

		str("HOST", &t.Host)
		str("DATABASE", &t.Database)
		str("APP_NAME", &t.AppName)
		str("CREDENTIAL_KEY", &t.CredentialKey)

		if v, k, o, ok := get("PORT"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 || n > 65535 {
				return nil, fmt.Errorf("%s=%q is not a valid port", k, v)
			}
			t.Port = n
			note(alias, "port", k, v, o)
		}

		if v, k, o, ok := get("ALLOW_CIDRS"); ok {
			list := splitList(v)
			if len(list) == 0 {
				// Fail closed, matching the policy file's own rule. An
				// override that blanks the allowlist must not read as
				// "no restriction".
				return nil, fmt.Errorf("%s is set but empty; an empty allowlist denies everything, "+
					"so state the networks explicitly or unset it", k)
			}
			t.AllowCIDRs = list
			note(alias, "allow_cidrs", k, strings.Join(list, ","), o)
		}

		encrypt := t.Encrypt == nil || *t.Encrypt
		if err := boolean("ENCRYPT", &encrypt); err != nil {
			return nil, err
		}
		t.Encrypt = &encrypt

		if err := boolean("TRUST_SERVER_CERTIFICATE", &t.TrustServerCertificate); err != nil {
			return nil, err
		}
		if err := boolean("WRITABLE", &t.Writable); err != nil {
			return nil, err
		}
	}

	p.applyDefaults()
	sort.Slice(applied, func(i, j int) bool {
		if applied[i].Alias != applied[j].Alias {
			return applied[i].Alias < applied[j].Alias
		}
		return applied[i].Field < applied[j].Field
	})
	return applied, nil
}

// extraTargets builds the targets named by ExtraTargetsKey.
//
// A target declared this way must supply host, database and allow_cidrs. There
// is no inheriting of another target's networks: a new endpoint that silently
// borrowed an existing allowlist would be a target nobody chose the blast
// radius for.
func extraTargets(src Source) ([]Target, error) {
	raw, _, ok := src.Lookup(ExtraTargetsKey)
	if !ok {
		return nil, nil
	}

	var out []Target
	seen := map[string]struct{}{}
	for _, alias := range splitList(raw) {
		if _, dup := seen[alias]; dup {
			return nil, fmt.Errorf("%s lists %q twice", ExtraTargetsKey, alias)
		}
		seen[alias] = struct{}{}

		t := Target{Alias: alias}
		for _, req := range []struct {
			field string
			dst   *string
		}{
			{"HOST", &t.Host},
			{"DATABASE", &t.Database},
		} {
			k := key(alias, req.field)
			v, _, found := src.Lookup(k)
			if !found {
				return nil, fmt.Errorf("target %q is declared in %s but %s is not set",
					alias, ExtraTargetsKey, k)
			}
			*req.dst = v
		}

		k := key(alias, "ALLOW_CIDRS")
		v, _, found := src.Lookup(k)
		if !found {
			return nil, fmt.Errorf("target %q is declared in %s but %s is not set; "+
				"a new target must state its own networks, it does not inherit any",
				alias, ExtraTargetsKey, k)
		}
		t.AllowCIDRs = splitList(v)

		// Note is filled in so a target that appeared from the environment is
		// distinguishable in reports from one somebody reviewed into the file.
		t.Note = "declared via " + ExtraTargetsKey
		out = append(out, t)
	}
	return out, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseBool accepts the spellings a person actually types in a .env file.
func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean (use true/false)", v)
}
