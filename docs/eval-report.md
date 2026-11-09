# Eval reports (M3-05)

`recon-eval report` renders the M3 evaluation results as Markdown and
`eval-report/v1` JSON. Reports are synthetic-data tooling: they read only the
durable `eval_runs` / `eval_case_results` rows and the committed dataset truth,
and they never write to the repository unless an explicit output path is given.

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

## JSON shape

Top level: `schema` (`eval-report/v1`), `generated_at`, `run`, `runs[]`,
`dataset`, `headline`, `by_mode`, `by_difficulty`, `repeats`, `calibration`,
`scorecards`, `failure_appendix`, `warnings`.

- `generated_at` comes from an injected clock and is the only volatile field;
  goldens pin it.
- `run`/`runs[]` are a whitelist of suite, split, dataset version, model,
  prompt version, seed, git SHA, timestamps, repeat fields, aborted/reason and
  cost cap. Arbitrary `eval_runs.config` keys and DSNs are never dumped.
- M3-05 leaves `calibration` null and `scorecards` empty. M3-06 fills them.
- `auto_resolve_precision` is deferred to M4-14.

Golden fixtures live in `eval/report/testdata/report.{md,json}`. Refresh them
only for an intentional output-contract change:

```sh
go test ./eval/report -run TestGoldenReport -update
```
