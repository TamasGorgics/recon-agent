# Eval baselines

This directory holds committed M3-09 baseline evidence: the one held-out `test`
run, its report, its metadata, and its record-only cassette sample. It is
evidence, not a replay input.

## M3-09 baseline

- **Status:** delivered. `eval/baselines/m3-09-test/` contains the single frozen
  `test` run (`8931b06c-a6b3-4049-a602-70ec69cbab0d`), report, metadata,
  self-gate output, and cassette sample.
- **Prompt freeze:** decided on `dev` only (v1 frozen; v2 accuracy fell
  3.47 pp). The selected prompt was never tuned against `test`.
- **Model:** `deepseek-v4.1-flash`.
- **Runtime:** exactly one `test` run, `--repeat 1`, within the $4.00 cap
  (actual spend $1.319247).
- **Gate:** the committed baseline passes `recon-eval gate --run <same run>
  --baseline <same run>` with exit 0.
- The `test` split is spent. Never rerun or resume it; a rerun invalidates the
  baseline, not just the budget.

## Record-only cassettes (Q-53/Q-68)

M3-08/M3-09 eval-run cassettes are **record-only evidence**. Case, break, run
and proposal IDs and scenario timestamps are database-generated and vary per
clone, so the cassettes are not byte-identical replay inputs. They are not
loaded by `TestCommittedCassettes` or any replay path, and they are not CI
inputs. Deterministic replay and the CI eval gate remain M6-05 and require a
pinned world.

## Size budget

- Committed cassette sample per baseline: **≤ 20 files**, **≤ 1 MiB per file**,
  deterministic sample documented in that baseline's `metadata.json`/README.
- Total committed cassette roots (`eval/cassettes`, `eval/cassette-evidence`,
  `eval/baselines/*/cassettes`): **≤ 10 MiB**.
- The M3-09 sample is 12 files / 623,701 bytes; the total is
  2,608,523 bytes of the 10,485,760-byte budget.
- The 12 M2 fixture cassettes are out of scope and must not change.

## Reproduction

The gate itself is offline against durable rows; it never calls a model. To
reproduce a gate decision from a database that holds the committed run:

```bash
go run ./cmd/recon-eval gate --run <candidate_run_id> --baseline <baseline_run_id>
```

Do not re-run the `test` split to reproduce evidence. M3-09 spends that run
exactly once.
