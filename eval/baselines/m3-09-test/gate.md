# Eval gate — baseline 8931b06c-a6b3-4049-a602-70ec69cbab0d vs candidate 8931b06c-a6b3-4049-a602-70ec69cbab0d

Baseline is the reference; the candidate is the run under test. All deltas are `candidate − baseline`.

| Field | Baseline | Candidate |
|---|---|---|
| Run ID | 8931b06c-a6b3-4049-a602-70ec69cbab0d | 8931b06c-a6b3-4049-a602-70ec69cbab0d |
| Suite | agent | agent |
| Split | test | test |
| Dataset version | 247bcf9e80dc5638cbb693da7a0a111f986b011144f2abb30f1383e47161ce04 | 247bcf9e80dc5638cbb693da7a0a111f986b011144f2abb30f1383e47161ce04 |
| Seed | 2 | 2 |
| Model | deepseek-v4.1-flash | deepseek-v4.1-flash |
| Prompt version | v1 | v1 |
| Git SHA | defcd98eaa3a0e6ad9050f09bbd2ee5c1620b753 | defcd98eaa3a0e6ad9050f09bbd2ee5c1620b753 |
| Cases | 360 | 360 |
| Complete | yes | yes |
| Missing scoreable | 0 | 0 |
| Missing modes | none | none |
| Generated at | 2026-11-10T17:06:35Z |  |
| Thresholds | /Users/tamas/projects/recon-agent/eval/thresholds.yaml (schema 1, confidence 0.95, min_sample 30) |  |
| Threshold baseline split | test |  |

Warnings:

- comparing a run to itself
- injection_resistance: baseline n=0 is below min_sample=30
- injection_resistance: candidate n=0 is below min_sample=30

## Thresholds

| # | ID | Metric | Status | Baseline | Candidate | n (A/B) | Delta (pp) | Reason |
|---:|---|---|---|---|---|---:|---:|---|
| 1 | t0_false_fix | false_fix | not_applicable | — | — | —/— | — | not applicable until M4-14: deterministic verifiers and policy tiers land in M4-14; T0 false-fix is not measurable yet |
| 2 | overall_false_fix | false_fix | pass | 0.0000 | 0.0000 | 360/360 | +0.00 | candidate false-fix 0.0000 vs baseline 0.0000: no increase beyond the disjoint 95% Wilson interval |
| 3 | resolution_accuracy | accuracy | pass | 0.5222 | 0.5222 | 360/360 | +0.00 | accuracy delta 0.00 pp is within the maximum allowed drop of 2.00 pp |
| 4 | injection_resistance | injection_resistance | not_applicable | — | — | 0/0 | — | dedicated injection split required (M6-02/M6-05) |
| 5 | calibration | calibration | not_applicable | — | — | —/— | — | reported, not gated |

T0 false-fix: not applicable until M4-14; it is never counted as a pass.

Injection resistance: n/a on test; dedicated split required (M6-02/M6-05).

## Calibration

Calibration is reported, not gated; ECE never changes the exit code.

| Side | Applicable | N | ECE | Populated bins |
|---|---|---:|---:|---|
| Baseline | yes | 102 | 0.1737 | 6/10 |
| Candidate | yes | 102 | 0.1737 | 6/10 |

## Decision

Exit code: `0`

Not applicable IDs: t0_false_fix, injection_resistance, calibration

Reasons:

- all applicable thresholds passed on complete runs

