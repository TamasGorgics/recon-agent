# Eval run comparison — c2fd2117-bbd1-400d-af01-8bfb4ac6b439 vs 91e15147-26fa-4625-84aa-1d07d3b224cf

A is the baseline; B is the candidate. All deltas are `B − A`.

| Field | A (baseline) | B (candidate) |
|---|---|---|
| Run ID | c2fd2117-bbd1-400d-af01-8bfb4ac6b439 | 91e15147-26fa-4625-84aa-1d07d3b224cf |
| Suite | agent | agent |
| Split | dev | dev |
| Dataset version | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 |
| Model | deepseek-v4.1-flash | deepseek-v4.1-flash |
| Prompt version | v1 | v2 |
| Seed | 1 | 1 |
| Git SHA | 7eded4c0f83c362177c6a8ace5353b2600ac97b3 | 7eded4c0f83c362177c6a8ace5353b2600ac97b3 |
| Repeat group | n/a | n/a |
| Repeat index | n/a | n/a |
| Cases | 144 | 144 |
| Proposals | 47 | 50 |
| Confidence observations | 47 | 50 |
| Complete | yes | yes |
| Missing scoreable | 0 | 0 |
| Missing modes | none | none |
| Aborted | no | no |
| Partial | no | no |

## Overall

Rates are proportions; brackets are 95% Wilson intervals. Delta is in percentage points (B − A). `no change` includes overlapping intervals even when the point delta is large.

| Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|
| Accuracy | 0.5347 | 0.5000 | -3.47 | [0.4534, 0.6142] | [0.4194, 0.5806] | yes | higher better | no change |
| False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.0260] | [0.0000, 0.0260] | yes | lower better | no change |
| Correct escalation | 0.6667 | 0.5909 | -7.58 | [0.5466, 0.7684] | [0.4705, 0.7013] | yes | higher better | no change |
| Over-escalation | 0.2692 | 0.1795 | -8.97 | [0.1834, 0.3768] | [0.1100, 0.2790] | yes | lower better | no change |
| Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.9244, 1.0000] | [0.9287, 1.0000] | yes | higher better | no change |
| Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## By failure mode

| Mode | Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|---|
| ambiguous | Accuracy | 0.8333 | 0.5833 | -25.00 | [0.5520, 0.9530] | [0.3195, 0.8067] | yes | higher better | no change |
| ambiguous | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| ambiguous | Correct escalation | 0.8333 | 0.5833 | -25.00 | [0.5520, 0.9530] | [0.3195, 0.8067] | yes | higher better | no change |
| ambiguous | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| ambiguous | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| ambiguous | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| batch_composite | Accuracy | 0.0833 | 0.1667 | +8.33 | [0.0149, 0.3539] | [0.0470, 0.4480] | yes | higher better | no change |
| batch_composite | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| batch_composite | Correct escalation | 0.1667 | 0.1667 | +0.00 | [0.0301, 0.5635] | [0.0301, 0.5635] | yes | higher better | no change |
| batch_composite | Over-escalation | 0.1667 | 0.1667 | +0.00 | [0.0301, 0.5635] | [0.0301, 0.5635] | yes | lower better | no change |
| batch_composite | Evidence validity | — | 1.0000 | n/a | n/a | [0.2065, 1.0000] | n/a | higher better | n/a |
| batch_composite | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| chargeback | Accuracy | 0.7500 | 0.6667 | -8.33 | [0.4677, 0.9111] | [0.3906, 0.8619] | yes | higher better | no change |
| chargeback | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| chargeback | Correct escalation | 0.7500 | 0.6667 | -8.33 | [0.4677, 0.9111] | [0.3906, 0.8619] | yes | higher better | no change |
| chargeback | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| chargeback | Evidence validity | — | 1.0000 | n/a | n/a | [0.2065, 1.0000] | n/a | higher better | n/a |
| chargeback | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| duplicate_charge | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| duplicate_charge | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| duplicate_charge | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| duplicate_charge | Over-escalation | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| duplicate_charge | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| duplicate_charge | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fee_variance | Accuracy | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| fee_variance | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| fee_variance | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fee_variance | Over-escalation | 0.4167 | 0.3333 | -8.33 | [0.1933, 0.6805] | [0.1381, 0.6094] | yes | lower better | no change |
| fee_variance | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.6457, 1.0000] | [0.6097, 1.0000] | yes | higher better | no change |
| fee_variance | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Accuracy | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| fx_rounding | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| fx_rounding | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Over-escalation | 0.2500 | 0.3333 | +8.33 | [0.0889, 0.5323] | [0.1381, 0.6094] | yes | lower better | no change |
| fx_rounding | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.6097, 1.0000] | [0.6097, 1.0000] | yes | higher better | no change |
| fx_rounding | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| injection_memo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| injection_memo | Correct escalation | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| injection_memo | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| injection_memo | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Accuracy | 0.8333 | 0.6667 | -16.67 | [0.5520, 0.9530] | [0.3906, 0.8619] | yes | higher better | no change |
| late_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| late_settlement | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Over-escalation | 0.0833 | 0.0000 | -8.33 | [0.0149, 0.3539] | [0.0000, 0.2425] | yes | lower better | no change |
| late_settlement | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.7412, 1.0000] | [0.6756, 1.0000] | yes | higher better | no change |
| late_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| missing_settlement | Accuracy | 0.0833 | 0.2500 | +16.67 | [0.0149, 0.3539] | [0.0889, 0.5323] | yes | higher better | no change |
| missing_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| missing_settlement | Correct escalation | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| missing_settlement | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| missing_settlement | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.2065, 1.0000] | [0.4385, 1.0000] | yes | higher better | no change |
| missing_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Accuracy | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| partial_refund | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| partial_refund | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Over-escalation | 0.7500 | 0.2500 | -50.00 | [0.4677, 0.9111] | [0.0889, 0.5323] | yes | lower better | no change |
| partial_refund | Evidence validity | — | 1.0000 | n/a | n/a | [0.4385, 1.0000] | n/a | higher better | n/a |
| partial_refund | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Accuracy | 0.8333 | 0.7500 | -8.33 | [0.5520, 0.9530] | [0.4677, 0.9111] | yes | higher better | no change |
| reference_typo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| reference_typo | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Over-escalation | 0.1667 | 0.1667 | +0.00 | [0.0470, 0.4480] | [0.0470, 0.4480] | yes | lower better | no change |
| reference_typo | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.7225, 1.0000] | [0.7009, 1.0000] | yes | higher better | no change |
| reference_typo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| unknown_deposit | Accuracy | 1.0000 | 0.9167 | -8.33 | [0.7575, 1.0000] | [0.6461, 0.9851] | yes | higher better | no change |
| unknown_deposit | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| unknown_deposit | Correct escalation | 1.0000 | 0.9167 | -8.33 | [0.7575, 1.0000] | [0.6461, 0.9851] | yes | higher better | no change |
| unknown_deposit | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| unknown_deposit | Evidence validity | — | 1.0000 | n/a | n/a | [0.2065, 1.0000] | n/a | higher better | n/a |
| unknown_deposit | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## Cost, latency, and steps

Descriptive only; these measurements never drive the exit code.

| Measure | A | B | Δ | Δ% |
|---|---|---|---|---|
| Cost mean (USD) | 0.003691 | 0.003795 | 0.000104 | +2.82 |
| Latency p50 (ms) | 0 | 0 | +0 | n/a |
| Latency p95 (ms) | 0 | 0 | +0 | n/a |
| Steps mean | 9.94 | 9.75 | -0.19 | — |

## Decision

**No gating regression.**

--report-only: exit suppressed (would exit 0).
