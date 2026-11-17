// Package flakes is the domain core of flake-fix workstreams: the evidence a
// check run records about its flaky checks, the per-project flake record kept
// in the project's trace, the hand-in key of a flake group and the hand-in the
// service writes from an occurrence's evidence. It reads no check runs and
// opens nothing; its callers do.
package flakes
