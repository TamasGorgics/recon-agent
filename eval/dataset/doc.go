// Package dataset builds the deterministic eval worlds and the versioned
// ground-truth documents consumed by the M3 eval runner and scorers.
//
// The package is deliberately DB-free and clock-free: Build is a pure function
// of Config, and the wall clock is never read. Generated worlds are transient;
// only eval/datasets/<split>/manifest.json is committed, while the world files
// contain hashes and counts sufficient to detect drift.
package dataset
