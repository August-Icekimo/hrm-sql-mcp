// Package target is the only place in this program that can produce a database
// connection.
//
// The design intent is structural rather than advisory. Target has no exported
// fields and no exported constructor, so no code outside this package can
// assemble one. The only way to obtain a *Target is Registry.Open, and Open
// always runs guard.check first. "Never connect to production" is therefore
// enforced by the type system: there is no code path that could skip the
// guard, not merely a comment asking callers not to.
//
// The guard fails closed at every step. Anything it cannot positively verify —
// an unresolvable hostname, an empty allowlist, a missing profile — is a
// rejection, never a pass.
package target
