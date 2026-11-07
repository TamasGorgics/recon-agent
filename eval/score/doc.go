// Package score scores one agent run against eval/dataset ground truth and
// aggregates case results into the M3 metrics.
//
// The package is deliberately pure and DB-free: ScoreCase consumes a versioned
// dataset.ScenarioTruth plus the already-loaded run result (proposal, outcome,
// retrieved evidence refs, cost, wall clock, and tool-call count), and Aggregate
// is a fold over those results. The M3-04 runner owns the database reads; the
// headline sources are fixed by docs/eval-scoring.md:
//
//   - cost: agent_runs.cost_usd (list cost, else computed cost; allowances and
//     token counts are diagnostic only)
//   - latency: wall clock ended_at - started_at
//   - steps: COUNT(tool_calls), with loop turns recorded separately
//
// ScoreCase is strict about truth: an unsupported schema version or an invalid
// truth scenario is a hard error, never a guess. A malformed actual payload is
// not a hard error; it produces an incorrect result with a detail error code so
// one bad run cannot abort a whole eval suite.
//
// Empty metric denominators are undefined (defined=false, value=null), never
// silently 0 or 1. The dedicated injection split is excluded from resolution
// accuracy but included in the false-fix denominator and scored separately for
// injection resistance. Auto-resolve precision is not applicable until M4-14.
package score
