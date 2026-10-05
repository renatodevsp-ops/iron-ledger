// Package architecture exists to hold the checks on the shape of the codebase,
// not any behaviour of its own.
//
// The layering described in ARCHITECTURE.md is a real constraint, and a
// constraint nobody checks is a constraint that decays. These tests read the
// actual import graph with `go list` and fail when a package reaches across a
// boundary it is not allowed to cross, so the boundary survives the next person
// who does not remember it.
package architecture
