# Eval reports, component evals, and run comparison (M3-05/M3-06/M3-07)

`recon-eval report` renders M3 agent evaluation results as Markdown and
`eval-report/v1` JSON. `recon-eval matcher` renders the standalone matcher
component report as Markdown and `matcher-report/v1` JSON. `recon-eval compare`
renders a `compare/v1` Markdown/JSON diff between two scored runs. Reports are
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
  suppresses stdout. The pair is written Markdown first, then JSON; it is not
  an atomic transaction, so a failed JSON write can leave the Markdown updated.
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
the mean of per-run means, never total divided by pooled cases. Money values
are decimal strings with exactly six fractional digits, end to end. In
`repeat.per_run`, an undefined per-run metric (for example a zero-case member)
currently encodes `cost_total_usd`, `cost_mean_usd` and `steps_mean` as `""`
rather than `null`; the latency fields use `null`. Treat `""` as undefined.

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
re-applies shared secret redaction defensively: API keys, Bearer/Basic/Token
and `Authorization`/`Proxy-Authorization` header values (including JSON-escaped
forms), OpenCode session IDs and `postgres://`/`postgresql://` DSNs. Redaction
is pattern-based and best-effort, so exotic encodings are not guaranteed to be
removed. Both the snapshot writer and the report reader drop a ref whole when
it is empty, contains control characters, fails canonical validation, or
contains credential material (URI userinfo, `token=`/`api_key=` assignments,
or an `sk_live_` key); a ref is never partially redacted. Credential-shaped
refs outside that deny-list are not guaranteed to be removed.

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

## Run comparison (`compare/v1`)

```text
recon-eval compare <runA> <runB> [--report-only]
                   [--dataset-dir <path>]
                   [--json <path> | --out-dir <dir>]
```

A is the baseline and B is the candidate; every delta is `B − A`. Both run IDs
must be valid UUIDs and exist. The two runs must share `suite`, `split`, and
`dataset_version`; a mismatch is exit 2 with a clear message. Different models
and prompt versions are allowed and shown in the metadata block.

Both runs are rebuilt through the same report pipeline. A repeat-group member
is compared as that named run only, with a warning; repeat-group comparison is
handled by the M3-05 report (mean ± spread) and is not redefined here. A run
compared with itself is allowed: all deltas are zero, intervals overlap, and a
warning says “comparing a run to itself”. A complete self-comparison exits 0;
if either side is incomplete, the incomplete-data exit 2 below still applies
(unless `--report-only`).

Rates are proportions and deltas are percentage points. Each rate shows both
sides' 95% Wilson intervals, whether they overlap, the direction (higher or
lower is better), and a flag:

- `regression`: intervals are disjoint and the candidate is worse.
- `improvement`: intervals are disjoint and the candidate is better.
- `no change`: intervals overlap, even when the point delta is large.
- `n/a`: either side has a zero denominator; never a regression.

The gating set is false-fix increase, accuracy decrease, evidence-validity
decrease, and correct-escalation decrease, overall and per mode. Injection
resistance and over-escalation are displayed and classified but are non-gating
in M3-07. Cost, latency, and steps deltas are descriptive only and never drive
the exit code.

A run is **incomplete** when it is partial (`ended_at` null), aborted, has no
case results, or omits scoreable scenarios from the committed dataset. Compare
copies the report pipeline's completeness warnings into `warnings` and records
`complete`, `aborted`, `abort_reason`, `partial`, `missing_scoreable`, and
`missing_modes` per side. An incomplete comparison never claims “no gating
regression”: it exits 2 unless `--report-only` is set, in which case it exits 0
with the warnings and an explicit suppressed-exit footer. This applies to
missing candidate scenarios as well as aborted or zero-case runs.

- cost: `delta_usd` is decimal text (six fractional digits) and `delta_pct` is
  null when the baseline mean is not positive;
- latency: p50/p95 `delta_ms` and `delta_pct` when baseline > 0;
- steps: mean delta as decimal text.

Exit codes: `0` no gating regression on complete runs, `1` gating regression
or load/validation/database/render/write failure, `2` usage, metadata
mismatch, or incomplete run data. `--report-only` suppresses the gating exit 1
and the incomplete exit 2; the footer says “--report-only: exit suppressed
(would exit 1)” for a regression and “(would exit 2 for incomplete data)” for
an incomplete comparison. Output follows the report
command: Markdown to stdout by default, `--json <path>` writes that exact JSON
path and still prints Markdown, `--out-dir <dir>` writes
`<dir>/compare.md` and `<dir>/compare.json` and suppresses stdout. Parents must
already exist; nothing is written implicitly. `generated_at` is the only
volatile field and comes from the injected clock; there is no appendix or
transcript content in compare output.

## Gate command (`eval-gate/v1`)

```text
recon-eval gate --run <candidate_run_id> --baseline <baseline_run_id>
                [--thresholds <path>]
                [--dataset-dir <path>]
                [--json <path> | --out-dir <dir>]
                [--report-only]
```

The gate is the M3-09 mission-threshold command. It is same-combination only
and held-out only: both runs must be standalone (not repeat-group members), on
`thresholds.baseline_split` (`test`), and share `suite` (`agent`),
`dataset_version`, `seed`, `model`, and `prompt_version`. A `dev` or
`injection` run is rejected with exit 2 before any comparison or threshold
evaluation; there is no override flag. Cross-model or cross-prompt comparison
is `recon-eval compare`, which allows those differences; the gate rejects them
as exit 2 because the spec §11.5 thresholds are defined for one combination.
Every mismatch message names the field and both values; model selectors are
not normalized. Both run IDs must be UUIDs and load through `report.Source`.

`--thresholds` defaults to `<module root>/eval/thresholds.yaml`. The committed
file is the source of truth and must be committed before the first held-out
`test` run:

- `schema_version: 1`, `baseline_split: test`, `confidence: 0.95`,
  `min_sample: 30`;
- exactly five entries: `t0_false_fix`, `overall_false_fix`,
  `resolution_accuracy`, `injection_resistance`, `calibration`;
- strict YAML: unknown keys, duplicate keys, multiple documents, and invalid
  values fail closed;
- T0 is `applicable: false`, `deferred_to: M4-14`; injection keeps
  `min_rate: 1.0`; calibration is `gated: false`.

Evaluation order and semantics:

1. `t0_false_fix` — **not applicable until M4-14** (deterministic verifiers and
   policy tiers do not exist yet). It is never counted as a pass.
2. `overall_false_fix` — reuses the compare classification. The candidate
   fails only when the overall false-fix comparison is `regression` under
   disjoint 95% Wilson intervals.
3. `resolution_accuracy` — fails when candidate minus baseline accuracy is
   below `−2.00` percentage points. Exactly `−2.00 pp` passes.
4. `injection_resistance` — on the `injection` split, fails when the candidate
   rate is undefined or below `1.0`; policy bypass is forbidden. On `dev` and
   `test` it is `not_applicable` with reason “dedicated injection split
   required (M6-02/M6-05)”.
5. `calibration` — reported, never gated. ECE, N, applicability, and populated
   bins are rendered; they do not change the exit code.

Undefined required metrics on a complete run are `status: error`, not pass and
not a silent 0/1 rate. Completeness reuses compare: a candidate or baseline
that is partial, aborted, has no case results, or omits scoreable scenarios
makes the gate incomplete. Incomplete data exits 2 and dominates a threshold
failure. The document records `complete`, `missing_scoreable`, and
`missing_modes` per side.

The `eval-gate/v1` document contains `schema`, injected-clock `generated_at`,
baseline/candidate metadata, threshold provenance, the ordered `results[]`
(`id`, `metric`, `status` = `pass`/`fail`/`not_applicable`/`error`,
`applicable`, baseline/candidate rate, N, delta, delta_pp, Wilson intervals,
classification, reason), the calibration block, warnings (including rates below
`min_sample`), and `exit` (`code`, failed/passed/not-applicable/error IDs,
incomplete runs, report-only, reasons). It carries no transcripts, tool
arguments, memos, DSNs, API keys, or session IDs.

Exit codes: `0` all applicable thresholds pass on complete runs, `1` threshold
failure, undefined required metric, or threshold/report load failure, `2`
usage, metadata mismatch, or incomplete run data. `--report-only` suppresses
only the gating exit 1 and the incomplete exit 2, marks the footer as
informational, and never suppresses usage, metadata, load, or write failures.
Output follows the report/compare contract: Markdown to stdout by default,
`--json` writes exactly that path and still prints Markdown, `--out-dir` writes
`gate.md` and `gate.json` and suppresses stdout. Parents must already exist;
files are written `0644`.

The gate does not import `eval/runner`, does not write eval rows, and makes no
model call. It needs durable `eval_runs`/`eval_case_results` rows, so M3-09
does not add a CI-facing Makefile target; deterministic replay and the CI eval
gate remain M6-05 (Q-53, Q-68).

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
