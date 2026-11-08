// Package runner implements `recon-eval run`: it validates a committed dataset
// manifest, loads one shared sandbox world per run/repeat, runs the M2 agent on
// each scoreable scenario, scores the persisted run facts, and writes durable
// `eval_runs` / `eval_case_results` rows.
//
// # Validation before side effects
//
// The committed manifest is loaded and the split is rebuilt before any sandbox
// or eval_runs row exists. A split, seed, dataset-version, days, difficulty, or
// truth-schema mismatch fails closed. Resume additionally requires the stored
// run's split/model/prompt/seed/dataset version to match the rebuilt dataset.
//
// # Sandbox and durability
//
// The results database is always the durable DATABASE_URL (or an injected test
// DSN); the sandbox clone is disposable and never stores eval results. Each
// run/repeat clones one world, maps scoreable scenarios to cases, runs the
// agent per case, and closes the clone after the pool drains. Results therefore
// survive sandbox teardown and remain SQL-queryable.
//
// # LLM modes
//
// Public LLM modes are `live` and `cassette`; an in-process scripted client
// exists only as a test seam. Live mode requires OPENCODE_API_KEY in the
// process environment and a positive cost cap; the key is never read from .env
// and never logged. Cassette mode performs no network calls and a missing
// fixture is a hard `cassette_miss` failure with no live fallback.
//
// # Repeats, resume, and cost cap
//
// Repeats are separate runs linked by repeat_group and repeat_index. Resume
// reuses an existing run id, skips scenarios that already have a result row,
// and is a no-op when the run is complete. The cost cap is checked before each
// case is scheduled against the persisted result-cost sum. The stop is
// intentional: the run is marked aborted with abort_reason=cost_cap, exit code
// is 0, and already-scheduled in-flight cases finish. With concurrency 1 the
// only overshoot is the single case accepted before the persisted sum crossed
// the cap; with concurrency N the bound is at most N in-flight cases. Resuming
// with a higher cap continues the run.
package runner
