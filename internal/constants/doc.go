// Package constants holds compile-time invariants that must not be
// configurable at runtime.
//
// The values here are deliberately not read from YAML or environment
// variables. Anything in this package can only be changed by editing the
// source, recompiling, and leaving a commit behind — which is exactly the
// friction we want in front of "which servers may this tool ever touch".
package constants
