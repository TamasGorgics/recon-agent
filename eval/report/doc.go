// Package report renders eval-report/v1 Markdown and JSON from durable
// eval_runs and eval_case_results rows.
//
// # Inputs and fail-closed checks
//
// Build loads one run by ID, or the whole repeat group when the requested run
// has repeat_group set. It then rebuilds the committed dataset split
// (dataset.LoadManifest + dataset.Build) and requires the manifest split,
// eval_dataset_version, seed, days, difficulty and truth schema to match the
// run and the rebuild. Scenario difficulty comes from that rebuilt truth; no
// difficulty column or migration exists.
//
// A result scenario absent from truth, a failure_mode/truth-mode mismatch, an
// undecodable detail document, a malformed cost, or an incomplete/inconsistent
// repeat group is a hard error. Truth scenarios not present in results are
// allowed for a partial/aborted run and reported as warnings.
//
// # Metrics
//
// Headline and per-mode/per-difficulty metrics reuse eval/score.Aggregate, so
// accuracy, false-fix, escalation, evidence-validity and injection-resistance
// denominators cannot drift from the pinned M3-03 scorer contract. Every rate
// carries a 95% Wilson interval and a minimum-sample flag
// (DefaultMinSample = 30).
//
// Repeat groups compute each metric per run first, then report mean, minimum,
// maximum and sample standard deviation. Per-run Wilson intervals stay
// visible; repeat cases are never pooled into one binomial sample and no
// Wilson interval is attached to a mean of rates. Cost is decimal text with
// six fractional digits end to end.
//
// # Calibration and component scorecards
//
// Calibration covers proposal rows with a non-null confidence and targets
// resolution correctness (never false-fix). It uses ten equal-width confidence
// bins with 1.0 in the last bin; bin assignment is decimal, while display
// values and ECE are float64. Empty observations are not applicable. A repeat
// group pools observations for the reliability table and lists per-run ECE.
// Calibration is descriptive and never gates.
//
// Every report carries one report-owned verifier scorecard. Options supplies an
// explicit VerifierSummary; when it is non-nil the card is applicable and
// reports total and per-pattern evaluated/passed/false-pass/false-negative
// counts plus pass rates. When the summary is nil the card is not applicable
// with reason "no verifier outcomes in this run". The report never imports
// internal/policy/verify or the DB service: the eval harness owns population.
//
// # Run comparison
//
// Compare builds each named run through the same report pipeline (never the
// repeat-group report aggregation), requires matching suite, split, and
// dataset_version, and renders compare/v1 Markdown + JSON from one model.
// Rate deltas are percentage points with both 95% Wilson intervals; a
// disjoint interval plus adverse direction is the only regression signal.
// False-fix increase and accuracy/evidence-validity/correct-escalation
// decreases gate exit 1, overall and per mode; injection resistance and
// over-escalation are classified but non-gating, and cost/latency/steps are
// descriptive. Build's completeness warnings (missing scoreable scenarios,
// partial/aborted state, zero cases, missing modes) are copied into the
// comparison, and either incomplete side makes the comparison exit 2 unless
// --report-only. --report-only suppresses the gating and incomplete exits; it
// never hides usage/metadata/load errors. Repeated runs are compared as the
// named run only, with a warning.
//
// # Transcripts and redaction
//
// The failure appendix renders only the sanitized, bounded version-1 snapshot
// stored by the runner in eval_case_results.detail["transcript"]. A missing
// snapshot means the run predates v1 and renders as unavailable; an unknown
// version renders as unsupported. The report never reads tool_call arguments,
// raw tool results, memo text, DSNs, API keys, or session identifiers. It
// re-applies the shared secret redaction and a 200-rune cap defensively.
//
// Snapshot sanitization is not full PII/PAN scrubbing. Use synthetic eval data
// only; M6-06 owns retention, redaction and PAN scanning.
package report
