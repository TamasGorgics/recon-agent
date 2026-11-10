# M3-09 frozen test baseline

This directory is the committed M3-09 held-out evidence for the `test` split.
It is **record-only evidence**, not a replay input (Q-53/Q-68).

## Run

| Field | Value |
|---|---|
| Run ID | `8931b06c-a6b3-4049-a602-70ec69cbab0d` |
| Suite / split | `agent` / `test` |
| Dataset version | `247bcf9e80dc5638cbb693da7a0a111f986b011144f2abb30f1383e47161ce04` (seed 2) |
| Model | `deepseek-v4.1-flash` |
| Prompt | `v1` (frozen) |
| Git SHA | `defcd98eaa3a0e6ad9050f09bbd2ee5c1620b753` (pre-squash) |
| Started / ended (UTC) | 2026-11-10T15:39:23Z / 2026-11-10T17:00:12Z |
| Completed | 360/360 scoreable cases, 0 skipped, not aborted |
| Exit | 0 |
| Spend / cap | **$1.319247 / $4.00** |
| Gate | self-gate exit **0**; `overall_false_fix` and `resolution_accuracy` pass; T0/injection/calibration n/a |

Command:

```bash
# OPENCODE_API_KEY must already be exported in the shell environment.
export DATABASE_URL='postgres://recon:recon@localhost:5432/recon?sslmode=disable'
go run ./cmd/recon-eval run --suite agent --split test \
  --model deepseek-v4.1-flash --prompt v1 --llm record \
  --cassette-dir eval/baselines/m3-09-test/cassettes \
  --max-cost-usd 4.00 --concurrency 4
```

The key is read from the process environment only; the runner does not read
`.env`, and no key value is recorded in this directory. The M3-09 recording was
a one-time human-approved exception that sourced a local `.env`; the key used
for that recording must be rotated. **The `test` split was executed exactly
once.** Do not rerun, resume, or tune prompts against it. A rerun invalidates
the baseline, not just the budget.

## Files

- `report.md`, `report.json` — `eval-report/v1` report for the run.
- `metadata.json` — run identity, dataset/threshold hashes, spend, timestamps,
  cassette sample policy, and gate result.
- `gate.md`, `gate.json` — captured self-gate document (`eval-gate/v1`).
- `cassettes/` — deterministic 12-file record-only sample.

## Prompt freeze

Freeze **v1**. On `dev`, A-v1 vs A-v2 accuracy was 0.5347 → 0.5000
(−3.47 pp), above the plan's 2 pp allowance, with no false-fix regression
either side. Model B (`glm-5.3-flash`, v1) was not frozen: it showed a
per-mode `unknown_deposit` regression. The freeze decision was made on `dev`
only; `test` was not consulted.

## Cassette sample (why only 12 files)

The run recorded **1,840 cassettes / 88,733,159 bytes**. Committing all of them
would exceed the total committed-cassette budget, so the baseline commits a
**deterministic sample of 12 files / 623,701 bytes** (max file 111,014 bytes):

> One cassette per failure mode: the lexicographically smallest SHA-256
> cassette filename among that mode's recorded calls. A cassette's mode is
> resolved from the first user message's scenario case id, mapped to
> `failure_mode` through the durable `eval_case_results` transcript's
> `case:<uuid>` ref. All 12 test-split modes are represented.

The sample is evidence coverage, not a replay set. Total committed cassette
roots (M2 fixtures + M3 evidence) are **2,608,523 bytes / 10,485,760 bytes**, so
the 10 MiB budget holds. Individual files are capped at 1 MiB by
`TestCommittedEvalCassetteBudget`.

## Gate

```bash
go run ./cmd/recon-eval gate \
  --run 8931b06c-a6b3-4049-a602-70ec69cbab0d \
  --baseline 8931b06c-a6b3-4049-a602-70ec69cbab0d
```

Exit 0. The gate reads `eval_runs` / `eval_case_results` and therefore needs
`DATABASE_URL`; `report.md` / `report.json` / `metadata.json` are readable
without a database. The M1 demo reset clears `eval_runs`, so the command above
is not rerunnable on the current database: the committed `gate.md` /
`gate.json` and the gate package tests are the surviving evidence, and the
`test` split must not be replayed to recreate rows. T0 false-fix is not
applicable until M4-14; injection resistance is not applicable on `test` and is
deferred to M6-02/M6-05; calibration is reported (ECE 0.1737, N=102), not gated.
