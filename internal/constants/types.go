package constants

// ProfileLocal targets the developer's podman container.
// ProfileUAT targets the shared UAT server.
//
// There is deliberately no production profile. Adding one is a source change,
// not a configuration change.
const (
	ProfileLocal = "local"
	ProfileUAT   = "uat"
)

// AllowedProfiles is the complete set of profiles this binary will run under.
//
// The profile must be supplied explicitly via HRM_SQL_MCP_PROFILE; there is no
// default. Requiring someone to write down which environment they are pointing
// at removes the most common way people connect to the wrong database: not
// realising they had to choose.
var AllowedProfiles = []string{ProfileLocal, ProfileUAT}

// DeniedHosts are hosts this tool must never connect to, matched after
// normalisation (see target.normaliseHost).
//
// 172.16.3.34 is the production FINANCIAL server. It holds live payroll and
// personal data for every employee. No amount of "the query is read-only"
// justifies reaching it from an agent-driven tool.
var DeniedHosts = []string{
	"172.16.3.34",
}

// DeniedCIDRs are networks this tool must never connect to.
//
// The host denylist above is not sufficient on its own: a CNAME, a hosts-file
// entry, or simply moving production to a new address would slip past a
// name-only check. Conversely a CIDR check alone would not stop someone
// pointing at a production box that lives outside the range. Both are applied,
// and either one rejecting is enough.
var DeniedCIDRs = []string{
	"172.16.3.0/24",
}

// DeniedProfileNames are profile values that must never be accepted even if
// someone adds them to a policy file. This is belt-and-braces on top of
// AllowedProfiles: a typo in AllowedProfiles could open a door, whereas a
// value appearing in both lists is a contradiction the guard rejects outright.
var DeniedProfileNames = []string{
	"prod", "production", "prd", "live",
}
