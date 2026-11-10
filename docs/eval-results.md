# Eval results — M3 agent (frozen test baseline)

Date: **2026-11-10** · Run: **`8931b06c-a6b3-4049-a602-70ec69cbab0d`**

| Field | Value |
|---|---|
| Split | `test` (held out) |
| Dataset version | `247bcf9e80dc5638cbb693da7a0a111f986b011144f2abb30f1383e47161ce04` (seed 2) |
| Model | `deepseek-v4.1-flash` |
| Prompt | `v1` (frozen) |
| Git SHA | `defcd98eaa3a0e6ad9050f09bbd2ee5c1620b753` (pre-squash) |
| Execution | `--repeat 1 --concurrency 4 --llm record`, $4.00 cap |
| Result | **360/360 scoreable cases**, 0 skipped, **not aborted**, exit 0 |
| Spend / runtime | **$1.319247** of $4.00 · 2026-11-10T15:39:23Z → 17:00:12Z (1h 20m 49s) |
| Cumulative eval spend | $2.815293 (M3-08 dev/aborted $1.496046 + this run) |
| Thresholds | `eval/thresholds.yaml` schema 1: 95% Wilson, min sample 30, baseline split `test` |
| Test-once | **One run only. No rerun, resume, or test-driven tuning.** |

Committed evidence: [`eval/baselines/m3-09-test/`](../eval/baselines/m3-09-test/README.md)
(`report.md`, `report.json`, `metadata.json`, `gate.md`, `gate.json`, cassette sample).

## Headline

| Metric | Value | 95% CI | n |
|---|---|---:|---:|
| Cases | 360 | — | — |
| Resolution accuracy | **0.5222** | [0.4707, 0.5733] | 188/360 |
| False-fix | **0.0000** | [0.0000, 0.0106] | 0/360 |
| Correct escalation | **0.6727** | [0.5979, 0.7397] | 111/165 |
| Over-escalation | **0.2667** | [0.2095, 0.3328] | 52/195 |
| Evidence validity | **1.0000** | [0.9637, 1.0000] | 102/102 |
| Injection resistance | n/a on `test` | — | 0/0 |
| Cost per case | $0.003665 | — | 360 |
| Latency p50 / p95 | 0 ms / 0 ms | — | 360 |
| Steps mean / total | 9.41 / 3388 | — | 360 |
| Auto-resolve precision | n/a until M4-14 | — | — |

The gate is configured with `overall_false_fix` (disjoint 95% Wilson, no
increase) and `resolution_accuracy` (point estimate, no drop > 2 pp). The
self-gate against the same run exits **0**; T0 false-fix is
`not_applicable` until M4-14, injection is `not_applicable` on `test`, and
calibration is reported but not gated.

## By failure mode

| Mode | Cases | Accuracy | False-fix | Correct esc. | Over esc. | Evidence |
|---|---:|---:|---:|---:|---:|---:|
| ambiguous | 30 | 0.9000 [0.7438, 0.9654] | 0.0000 | 0.8333 [0.6644, 0.9266] | n/a | 1.0000 (n=2) ⚠ |
| batch_composite | 30 | 0.1000 [0.0346, 0.2562] | 0.0000 | 0.2000 [0.0705, 0.4519] | 0.4000 [0.1982, 0.6425] | n/a (n=15) ⚠ |
| chargeback | 30 | 0.6333 [0.4551, 0.7813] | 0.0000 | 0.6333 [0.4551, 0.7813] | n/a | n/a |
| duplicate_charge | 30 | 0.9667 [0.8333, 0.9941] | 0.0000 | n/a | 0.0333 [0.0059, 0.1667] | 1.0000 (n=29) ⚠ |
| fee_variance | 30 | 0.0000 [0.0000, 0.1135] | 0.0000 | n/a | 0.5333 [0.3614, 0.6977] | 1.0000 (n=12) ⚠ |
| fx_rounding | 30 | 0.0000 [0.0000, 0.1135] | 0.0000 | n/a | 0.2667 [0.1418, 0.4445] | 1.0000 (n=9) ⚠ |
| injection_memo | 30 | 1.0000 [0.8865, 1.0000] | 0.0000 | 1.0000 [0.8865, 1.0000] | n/a | n/a |
| late_settlement | 30 | 0.5667 [0.3920, 0.7262] | 0.0000 | n/a | 0.1000 [0.0346, 0.2562] | 1.0000 (n=17) ⚠ |
| missing_settlement | 30 | 0.2000 [0.0951, 0.3731] | 0.0000 | 0.2000 [0.0951, 0.3731] | n/a | n/a |
| partial_refund | 30 | 0.0000 [0.0000, 0.1135] | 0.0000 | n/a | 0.5667 [0.3920, 0.7262] | 1.0000 (n=2) ⚠ |
| reference_typo | 30 | 0.9667 [0.8333, 0.9941] | 0.0000 | n/a | 0.0333 [0.0059, 0.1667] | 1.0000 (n=29) ⚠ |
| unknown_deposit | 30 | 0.9333 [0.7868, 0.9815] | 0.0000 | 0.9333 [0.7868, 0.9815] | n/a | 1.0000 (n=2) ⚠ |

All modes have exactly 30 cases, so no mode is below the 30-case floor. ⚠ marks
modes where at least one **metric denominator** (evidence/escalation subset) is
below 30 — the report's min-sample warning fires on that sparse denominator,
not on the mode count. Treat those per-mode intervals as descriptive only.

**By difficulty:** the committed `test` manifest is a single `realistic`
difficulty, 360 cases, accuracy 0.5222 [0.4707, 0.5733].

## Calibration (reported, not gated)

- **N = 102** proposal-confidence observations; **ECE = 0.1737**; 10 equal-width
  bins, 6 non-empty.
- High-confidence proposals are mostly right: bin [0.9, 1.0] has n=59,
  mean confidence 0.9200, accuracy 1.0000 (gap 0.08).
- Mid-confidence proposals are overconfident: [0.6, 0.7) n=10 accuracy 0.1000
  (gap 0.51); [0.7, 0.8) n=6 accuracy 0.0000 (gap 0.72).
- Calibration never changes the gate exit code (threshold `gated: false`).

## Dev comparisons and prompt freeze

Three `dev` runs (144 cases each) were recorded in M3-08 against the same
dataset family (dev dataset version
`8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411`, seed 1);
`test` was not used for any selection.

| Comparison | Accuracy | False-fix | Correct esc. | Over esc. | Evidence | Per-mode gate |
|---|---|---:|---:|---:|---:|---|
| A-v1 (`deepseek-v4.1-flash`, v1) | 0.5347 | 0.0000 | 0.6667 | 0.2692 | 1.0000 | — |
| A-v2 (`deepseek-v4.1-flash`, v2) | 0.5000 | 0.0000 | 0.5909 | 0.1795 | 1.0000 | — |
| B-v1 (`glm-5.3-flash`, v1) | 0.5972 | 0.0000 | 0.5606 | 0.1923 | 1.0000 | — |
| A-v1 → A-v2 | **−3.47 pp** | 0 → 0 | −7.58 pp | −8.97 pp | 0 | no regression |
| A-v1 → B-v1 | +6.25 pp | 0 → 0 | −10.61 pp | −7.69 pp | 0 | **regression: `unknown_deposit` accuracy and correct escalation 1.0000 → 0.5000** |
| A-v2 → B-v1 | +9.72 pp | 0 → 0 | — | — | 0 | no regression |

**Freeze v1.** v2 had no false-fix regression, but its dev accuracy fell
**3.47 pp**, beyond the plan's 2 pp allowance, so the v2-freeze condition failed.
Model B was not a candidate for the frozen baseline because its dev comparison
showed a disjoint-Wilson per-mode regression in `unknown_deposit`. No prompt or
model was changed after the test run. Dev spend: A-v1 $0.531464 (plus one
aborted $0.087183 attempt), A-v2 $0.546491, B-v1 $0.330880.

## Known failures

Outcome distribution across 360 cases: **102 proposals, 163 escalations,
72 budget-exhausted, 23 invalid-output**.

- The dominant quality gap is adjustment handling: `fee_variance`,
  `fx_rounding`, and `partial_refund` scored **0/30**; `batch_composite` 3/30
  and `missing_settlement` 6/30. These never produced a correct `adjustment`
  proposal — recorded resolutions were `request_info`, `escalate`, or no
  terminal proposal. `chargeback` (19/30) and `late_settlement` (17/30) were
  the next weakest.
- `budget_exhausted` (72/360) is concentrated in `missing_settlement` (24),
  `batch_composite` (16), `fx_rounding` (12), and `chargeback` (11). The
  per-run agent budget (defaults: 15 steps, 15 tool calls, 100,000 total
  tokens, $0.50, 5 minutes) stopped those runs before a terminal decision.
- `invalid_output` (23/360) is concentrated in `late_settlement` (9),
  `partial_refund` (6), and `batch_composite` (5).
- Injection-memo cases all escalated (30/30), which is the expected `test`
  behavior but is **not** an injection-resistance measurement.
- The report's failure appendix is bounded to 20 locator rows (sanitized
  transcript snapshots, no raw memos); use `report.json` for the full metric
  surface and the durable rows for deeper failure analysis in M6-08.

## Limitations

- **T0 false-fix is not applicable until M4-14.** The threshold is declared
  `applicable: false`; it is never counted as a pass.
- **Injection resistance is not applicable on `test`.** It is gated only on the
  dedicated `injection` split and deferred to M6-02/M6-05.
- **Single run (`k=1`).** No repeat variance or pooled interval is claimed.
  Per-mode n=30 intervals are wide.
- **Latency is 0 ms by construction.** Eval runs use the deterministic fake
  scenario clock, so `agent_runs.ended_at − started_at` is zero; real LLM/tool
  durations are recorded only in case detail. Do not read the 0 ms as a
  performance measurement.
- **Record-only cassettes (Q-53/Q-68).** Database-generated ids and timestamps
  prevent byte-identical replay. The committed baseline contains a deterministic
  12-file, 623,701-byte sample (one file per failure mode) of the 1,840-file
  recording; it is evidence, not a replay set. Deterministic replay and the CI
  eval gate remain M6-05.
- **Cassette budget.** Total committed cassette roots are 2,608,523 bytes of the
  10,485,760-byte budget; every committed file is ≤ 1 MiB
  (`TestCommittedEvalCassetteBudget`).
- **Gate needs durable rows.** `recon-eval gate --run` reads `DATABASE_URL`;
  the committed report/metadata do not.
- **No tuning on `test`.** Findings above are recorded for M6-08 / a later
  scoped change; the frozen baseline is not a prompt-selection signal.
