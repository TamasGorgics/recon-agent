// Package untrusted implements structural separation, sanitization, and
// detection for untrusted payment data (spec section 9 items 1, 4, 5; roadmap
// M2-07).
//
// The package is pure standard-library code: it does not import
// internal/agent/tools and compiles without a database. M2-09 uses a per-run
// Wrapper to place an untrusted value inside a labelled block that carries an
// unguessable boundary token. Before wrapping, the Sanitizer strips control,
// bidi and zero-width characters and bounds the value by rune count. The
// Detector flags instruction-like content so the runner can persist
// access-restricted flags and force a run to escalate-only.
//
// Nothing in this package logs values, matched text, or the boundary token.
package untrusted
