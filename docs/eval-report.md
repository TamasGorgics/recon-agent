# Eval reports and component evals (M3-05/M3-06)

`recon-eval report` renders M3 agent evaluation results as Markdown and
`eval-report/v1` JSON. `recon-eval matcher` renders the standalone matcher
component report as Markdown and `matcher-report/v1` JSON. Reports are
synthetic-data tooling: they read only the durable `eval_runs` /
`eval_case_results` rows (agent reports) or a disposable eval sandbox clone
(matcher report) plus the committed dataset truth, and they never write to the
repository unless an explicit output path is given.

## Command

```text
recon-eval report --run <eval_run_id>
                  [--dataset-dir <path>]
                  [--json <path> | --out-dir <dir>]
                  [--appendix-limit <n>]   # default 20
```

- Markdown goes to stdout by default.
- `--json <path>` writes JSON to exactly that path and still prints Markdown.
- `--out-dir <dir>` writes `<dir>/report.md` and `<dir>/report.json` and
  suppresses stdout.
- `--json` and `--out-dir` are mutually exclusive. Parent directories must
  already exist; reports are never written implicitly.
- `DATABASE_URL` is the durable results database. `--dataset-dir` defaults to
  `<module root>/eval/datasets/<split>`.

Exit codes: `0` report rendered (including an existing zero-case run, with a
warning), `1` load/validation/database/render/write failure, `2` usage.

## Matcher command

```text
recon-eval matcher --split dev|test|injection
                   [--dataset-dir <path>]
                   [--json <path> | --out-dir <dir>]
```

- `--split` is required; `dev` and `test` are the acceptance splits and
  `injection` is supported for symmetry.
- The committed manifest is loaded, the split is rebuilt, and drift in version,
  split, seed, days, difficulty, or truth schema fails closed before any
  sandbox is created.
- A disposable `eval/sandbox` clone loads the world and runs the production
  matcher. Active matches are read through `sandbox.App` (`recon_app`); the
  command never falls back to the admin pool for data reads and adds no grants.
- The report is standalone: no `eval_runs` or `eval_case_results` rows are
  written. The sandbox admin DSN is `TEST_DATABASE_URL`, falling back to
  `DATABASE_URL`.
- Output follows the report command: Markdown to stdout by default,
  `--json <path>` writes that exact JSON path and still prints Markdown,
  `--out-dir <dir>` writes `<dir>/matcher.md` and `<dir>/matcher.json` and
  suppresses stdout. Parents must already exist; files are `0644`.
- Exit `0` when the report is produced (recall below 1.0 is a finding), `1` on
  infrastructure/validation/load/render failure, `2` on usage.

## Fail-closed inputs

The report loads the requested run by ID. When `repeat_group` is set it loads
the whole group ordered by `repeat_index` and requires a complete contiguous
`0..k-1` set sharing suite, split, dataset version, model, prompt version and
seed. A missing or inconsistent sibling is an error; a differing `git_sha` is
allowed with a warning.

The committed split is rebuilt with `eval/dataset` and verified against the
manifest and the run:

- manifest split equals the run split;
- manifest `eval_dataset_version` equals the run's `dataset_version`;
- the rebuilt manifest matches the committed version, split, seed, days,
  difficulty and truth schema (the same drift set as the runner);
- every result scenario exists in truth with the same failure mode;
- `detail` must decode as a JSON object.

Truth scenarios without results produce a partial/incomplete warning; they are
not silently used to change a denominator. Difficulty comes from the rebuilt
truth, not from a database column.

## Metrics

The headline and every per-failure-mode/per-difficulty block reuse
`eval/score.Aggregate`, so the denominators are the pinned M3-03 contract:

| Metric | Definition |
|---|---|
| Accuracy | correct non-injection-split cases / all non-injection-split cases |
| False-fix | false-fix cases / all cases |
| Correct escalation | escalated cases where truth allows escalate / cases where truth allows escalate |
| Over-escalation | escalated cases with a resolvable truth / resolvable cases |
| Evidence validity | proposals with valid evidence / proposals |
| Injection resistance | safe injection-split cases / injection-split cases |
| Cost | decimal USD total and mean from `eval_case_results.cost_usd` |
| Latency | nearest-rank p50/p95 wall-clock milliseconds |
| Steps | tool-call total and mean |

The dedicated `injection` split is excluded from accuracy, and injection
resistance is defined only for that split (`n/a` elsewhere). A dev/test report
still includes `injection_memo` rows in accuracy, as the scorer does.

Every rate is a report-local wrapper around `score.Rate`; `eval/score.Rate` and
`eval/score.Report` are unchanged.

## Wilson intervals

Rates carry a two-sided 95% Wilson score interval with
`z = 1.959963984540054`. The formula is the standard Wilson center/half-width
formula, clamped to `[0, 1]` only for floating-point cleanup. `n=0` is
undefined (`defined=false`, `value`/`wilson_*` null), never 0 or 1. Reference
points include `0/30` upper `≈0.1135`, `30/30` lower `≈0.8865`, and `15/30`
`≈[0.3315, 0.6685]`.

## Repeats

A repeat group is reported as a group by default. Each metric is computed per
run first; the group value is the mean of the per-run values, with minimum,
maximum and sample standard deviation (`k-1` denominator). Repeat cases are
never pooled into one binomial sample, and no Wilson interval is attached to a
mean of rates. Per-run numerators/denominators and per-run Wilson intervals
remain visible in `repeat.per_run` and the Repeats table.

For cost, the group total is the sum of per-run totals and the group mean is
the mean of per-run means, never total divided by pooled cases. Money is always
a decimal string with exactly six fractional digits, end to end.

## Minimum sample

`min_sample` is 30 by default. Every rate whose own denominator is below 30 is
flagged in JSON (`min_sample_met=false`) and Markdown (`⚠ n=… < 30`); the
metric is rendered, not suppressed. A report warning summarises flagged
failure modes and difficulties. A mode's scoreable-case denominator is the
meaningful one: `batch_composite` has 15 test applications but 30 scoreable
cases.

## Failure appendix

The appendix lists up to `--appendix-limit` failures (default 20), ordered:

1. false-fix cases;
2. other incorrect cases, including malformed-actual `detail.error`;
3. over-escalations;
4. provider-error / budget-exhausted / invalid-output cases.

Ties break by mode, scenario ID and repeat index. A repeat group lists a
scenario once, counts `repeats_failed` out of `k`, and uses the first failing
repeat's transcript as the example. Rows include the scenario, mode,
difficulty, outcome, expected vs actual resolution, correctness flags,
confidence, evidence counts, error code, `agent_run_id`, `proposal_id` and the
transcript status.

The only transcript content is the sanitized, bounded version-1 snapshot the
runner stored in `eval_case_results.detail["transcript"]`: locator, proposal
summary, a redacted ≤200-rune assistant excerpt, and ordered tool steps with
only tool name, canonical refs, error code and truncation flag. The report
re-applies shared secret redaction (API keys, bearer/authorization headers,
OpenCode session IDs and `postgres://`/`postgresql://` DSNs) defensively.

- Missing snapshot: "transcript unavailable (run predates snapshot v1)" and
  the locator; this is not a report failure.
- Unknown snapshot version: warning and an unsupported marker; the snapshot is
  never parsed with guessed semantics.
- Raw tool arguments, raw tool results, raw memos, DSNs, API keys and session
  identifiers are never read or rendered.

Snapshot sanitization is **not** full PII/PAN scrubbing. Use synthetic eval
data only; M6-06 owns retention, redaction and PAN scanning. Reports go only to
explicit paths so excerpts are not committed accidentally.

## Calibration

The calibration section covers proposal rows only: every
`eval_case_results` row with a non-null `confidence`. Rows without a proposal
(escalation, budget exhaustion, provider error) have null confidence and are
excluded; the report states `N` so the proposals-only denominator is explicit.
The target is resolution `correct`, never false-fix.

Confidence is split into ten equal-width bins `[0.0,0.1) … [0.9,1.0]`, with
confidence exactly `1.0` in the last bin. Bin assignment uses decimal
arithmetic (`floor(confidence × 10)`, clamped to 9); `float64` is used only for
display values and ECE. Each bin carries `count`, `mean_confidence`, `accuracy`
and `gap = |accuracy − mean_confidence|`; empty bins carry `null`, never zero.
`ECE = Σ (count_b / N) · gap_b`.

- `N=0` is not applicable (`applicable=false`, `ece=null`, empty table).
- A single run has `scope="run"`.
- A repeat group pools observations for its reliability table and lists each
  run's ECE in `per_run_ece` (`scope="repeat_group"`).
- Calibration is descriptive and never gates.

## Component scorecards

Every M3-06 agent report contains one generic verifier placeholder:

```json
{
  "name": "verifier",
  "applicable": false,
  "deferred_to": "M4-05",
  "reason": "deterministic verifiers land in M4-05",
  "cases": 0,
  "metrics": []
}
```

Markdown renders "not applicable until M4-05". M3-06 does not import or create
`internal/policy/verify`; M4-05 fills the same report-owned scorecard shape.

## JSON shape

Top level: `schema` (`eval-report/v1`), `generated_at`, `run`, `runs[]`,
`dataset`, `headline`, `by_mode`, `by_difficulty`, `repeats`, `calibration`,
`scorecards`, `failure_appendix`, `warnings`.

- `generated_at` comes from an injected clock and is the only volatile field;
  goldens pin it.
- `run`/`runs[]` are a whitelist of suite, split, dataset version, model,
  prompt version, seed, git SHA, timestamps, repeat fields, aborted/reason and
  cost cap. Arbitrary `eval_runs.config` keys and DSNs are never dumped.
- `calibration` is always present from M3-06, with `applicable=false` when
  there are no confidence observations. `scorecards` contains the verifier
  placeholder.
- `auto_resolve_precision` is deferred to M4-14.

## Matcher report (`matcher-report/v1`)

Top level: `schema`, `generated_at`, `split`, `dataset_version`, `scenarios`,
`scoreable_scenarios`, `matches`, `correct_matches`, `precision`,
`expected_links`, `recalled_links`, `recall`, `per_pass`, `excluded`,
`warnings`.

- `scenarios` counts every truth row (clean rows included); the matcher scores
  clean links too. `scoreable_scenarios` uses the eval-runner definition
  (non-empty break signature or `injection_memo`) so the two denominators are
  not confused.
- `per_pass` is sorted by pass name. Per-pass recall uses the global
  required-link denominator (M1-16), so the per-pass recalls sum to overall
  recall.
- Precision with zero active matches is `defined=false`/`value=null` (`n/a`),
  not a misleading 1.0; recall with zero required links is likewise `n/a`, not
  0.0. Each zero denominator adds a warning. The underlying
  `internal/matchscore.Score` contract is unchanged.
- `excluded` always contains `ambiguous`, `escalate`, `no_external_record`,
  `no_ledger_txn` and `not_expected_link` (zero when absent).
- Recall below 1.0 is a finding, not a command failure. Known ADR 0004
  residuals appear as real misses; the report does not hard-code counts.

Golden fixtures live in `eval/report/testdata/report.{md,json}` and
`eval/component/testdata/matcher.{md,json}`. Refresh them only for an
intentional output-contract change:

```sh
go test ./eval/report -run TestGoldenReport -update
go test ./eval/component -run TestGoldenMatcher -update
```
