// Package matchscore measures the deterministic matcher against synthetic
// ground truth.
//
// One match group is one active matches row plus its external record and its
// active match_legs. Precision is correct groups divided by all active groups.
// A group is correct when its external record belongs to a ground-truth
// scenario with both sides and its ledger set exactly equals that scenario's
// ledger_txn_ids. The match rule and the scenario's primary resolution do not
// change correctness: a fee-variance tolerance match is still the right pair,
// and HD-3 allows a partial_refund link even though its primary resolution is
// adjustment.
//
// Recall's denominator is the set of ground-truth scenarios that allow "link"
// and have both an external and a ledger side. Ambiguous scenarios and any
// scenario whose expected resolutions include "escalate" are excluded and
// counted in Result.Excluded. The same exclusion is reported by the demo
// summary and recorded in ADR 0004 so the denominator is auditable.
//
// Scores are deterministic integers divided once at the end; no money value
// ever goes through floating point. Result.PerPass attributes each active group
// to its rule and reports per-pass precision and recall contribution.
//
// The package also provides the M1-16 annotation scanner. It walks *_test.go
// files, parses `// covers: FIN-xx` comments, and reports missing IDs for the
// documented subset (FIN-01..06, 10..14, 50, 51, 55). Full FIN coverage is
// M6-07's checker.
package matchscore
