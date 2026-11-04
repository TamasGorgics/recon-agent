# 0004 — M1 gate thresholds and measured baseline

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

M1-16 is the M1 exit gate. The roadmap sets placeholders (precision ≥ 0.99,
recall ≥ 0.95) and requires the measured values to be recorded here. The gate
must be reproducible from a fixed demo configuration, and it must state which
FIN requirements it actually asserts (Q-03 / D-8).

The measurement uses the same path as `make demo-m1`:

- `recon demo seed --seed 1 --days 14 --difficulty realistic --reset`
- `internal/demo.DefaultModeConfig()` = `modes.DefaultAllConfig(24)`, which
  spreads 24 applications over all twelve M1 failure modes.
- PostgreSQL 16.15 (docker compose), migrations through
  `20261104140000_breaks_cases.sql`.
- `TestMilestone1Gate` in `internal/matchscore/gate_test.go` (integration tag),
  run on 2026-11-03.

## Decision

### Thresholds

| Metric | Threshold | Measured | Result |
| --- | --- | --- | --- |
| Match precision | ≥ 0.99 | 1.0000 (363/363 groups) | pass |
| Match recall (matchable pairs) | ≥ 0.95 | 0.9890 (358/362 links) | pass |

Both thresholds are assertions in `TestMilestone1Gate`; the gate fails below
them. The demo also asserts a non-zero recall denominator.

### Measured baseline

- Seed 1, `realistic`, 14 days, dataset version
  `d668c4a93c13743d33d482677574a9ef91f2dd83d315f78ed0e7d3ac28a73237`.
- 8 scopes, 363 active matches, 363 correct, precision 1.0000.
- 362 required links, 358 recalled, recall 0.9890.
- Per pass: `exact` 358/358 correct and 358 required hits; `reference_tolerance`
  4/4 correct; `fuzzy` 1/1 correct. Per-pass recall is reported against the
  global denominator, so the last two passes show 0.0000 by construction:
  their links are already counted by `exact`.
- 6 breaks (2 duplicate, 4 external-only), 6 open cases.
- Gate run runtime: **1.768 s** for the demo pipeline inside
  `TestMilestone1Gate`; the full integration package took 2.430 s. `make
  demo-m1` takes a few seconds including goose migrations.
- Two consecutive `make demo-m1` runs (the target already passes `--reset`)
  printed identical summary lines and the same dataset version.

### Residual misses (recorded, not hidden by the threshold)

Four required links are not recalled:

1. Two `partial_refund` scenarios remain unmatched. Their ground truth allows
   `[link, adjustment]` (HD-3); the M1 matcher does not yet produce the
   adjustment path. This is the expected M1 limitation and belongs to the M3
   scorer/agent work.
2. Two `batch_composite` link scenarios are not matched. The generator emits a
   negative payout against positive charge legs whose magnitude equals the
   payout; `internal/match` pass 3 requires an exact **signed** sum, so the
   candidate is rejected and the payout becomes an external-only break. This is
   a pre-existing M1-10/M1-13 sign-convention gap, not an M1-16 regression and
   not a reason to change the thresholds. It is deferred; M3 calibration and
   the M6 gate must re-measure it.

The continuity bug fixed by M1-16 (an injected `unknown_deposit` broke later
same-currency statement continuity, causing M1-07 to quarantine 38 bank rows)
was the source of the earlier 0.8785 recall. With continuity restored the
figures above are the valid M1 baseline.

### Subset-gate rationale (Q-03 / D-8)

`TestMilestone1Gate` asserts annotated tests only for **FIN-01 through FIN-06,
FIN-10 through FIN-14, FIN-50, FIN-51, and FIN-55**. This is deliberate: it is
the documented M1 subset gate, not a claim of full FIN coverage. M1 tests also
exercise FIN-24, FIN-30/31/33, FIN-40/41/42/43, FIN-54, and FIN-56, but those
annotations are outside the gate assertion. The full requirement coverage
checker (`make fin-coverage`) lands in M6-07, and missing annotations inside the
M1 subset fail M1-16 while missing annotations outside it do not.

### Re-measurement

- **M3-06** (component evals and calibration) re-runs this configuration with
  the M3 scorer and records calibrated precision/recall per pass.
- **M6-07** re-runs the completed coverage checker and the full gate; any
  threshold or dataset change is a new decision record that supersedes this
  one, not an edit to these numbers.

## Consequences

- The M1 gate is reproducible from seed 1, `realistic`, 14 days, and stays
  green at the recorded values.
- A future change that drops precision below 0.99 or recall below 0.95 fails
  `TestMilestone1Gate` and CI rather than silently degrading the demo.
- The two `batch_composite` misses are visible in the ADR and in the demo
  summary (as external-only breaks), so M3/M6 must address or explicitly accept
  them instead of discovering them late.
- The subset rationale is now recorded in both the gate test and this ADR, so
  Q-03 is resolved without claiming coverage the M1 tests do not assert.
