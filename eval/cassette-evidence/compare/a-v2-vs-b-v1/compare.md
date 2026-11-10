# Eval run comparison — 91e15147-26fa-4625-84aa-1d07d3b224cf vs 2fc6b629-0545-42cb-a7c4-9c39af548b8e

A is the baseline; B is the candidate. All deltas are `B − A`.

| Field | A (baseline) | B (candidate) |
|---|---|---|
| Run ID | 91e15147-26fa-4625-84aa-1d07d3b224cf | 2fc6b629-0545-42cb-a7c4-9c39af548b8e |
| Suite | agent | agent |
| Split | dev | dev |
| Dataset version | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 |
| Model | deepseek-v4.1-flash | glm-5.3-flash |
| Prompt version | v2 | v1 |
| Seed | 1 | 1 |
| Git SHA | 7eded4c0f83c362177c6a8ace5353b2600ac97b3 | 7eded4c0f83c362177c6a8ace5353b2600ac97b3 |
| Repeat group | n/a | n/a |
| Repeat index | n/a | n/a |
| Cases | 144 | 144 |
| Proposals | 50 | 76 |
| Confidence observations | 50 | 76 |
| Complete | yes | yes |
| Missing scoreable | 0 | 0 |
| Missing modes | none | none |
| Aborted | no | no |
| Partial | no | no |

## Overall

Rates are proportions; brackets are 95% Wilson intervals. Delta is in percentage points (B − A). `no change` includes overlapping intervals even when the point delta is large.

| Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|
| Accuracy | 0.5000 | 0.5972 | +9.72 | [0.4194, 0.5806] | [0.5156, 0.6738] | yes | higher better | no change |
| False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.0260] | [0.0000, 0.0260] | yes | lower better | no change |
| Correct escalation | 0.5909 | 0.5606 | -3.03 | [0.4705, 0.7013] | [0.4408, 0.6737] | yes | higher better | no change |
| Over-escalation | 0.1795 | 0.1923 | +1.28 | [0.1100, 0.2790] | [0.1202, 0.2933] | yes | lower better | no change |
| Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.9287, 1.0000] | [0.9519, 1.0000] | yes | higher better | no change |
| Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## By failure mode

| Mode | Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|---|
| ambiguous | Accuracy | 0.5833 | 0.8333 | +25.00 | [0.3195, 0.8067] | [0.5520, 0.9530] | yes | higher better | no change |
| ambiguous | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| ambiguous | Correct escalation | 0.5833 | 0.3333 | -25.00 | [0.3195, 0.8067] | [0.1381, 0.6094] | yes | higher better | no change |
| ambiguous | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| ambiguous | Evidence validity | — | 1.0000 | n/a | n/a | [0.6097, 1.0000] | n/a | higher better | n/a |
| ambiguous | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| batch_composite | Accuracy | 0.1667 | 0.4167 | +25.00 | [0.0470, 0.4480] | [0.1933, 0.6805] | yes | higher better | no change |
| batch_composite | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| batch_composite | Correct escalation | 0.1667 | 0.3333 | +16.67 | [0.0301, 0.5635] | [0.0968, 0.7000] | yes | higher better | no change |
| batch_composite | Over-escalation | 0.1667 | 0.1667 | +0.00 | [0.0301, 0.5635] | [0.0301, 0.5635] | yes | lower better | no change |
| batch_composite | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.2065, 1.0000] | [0.5655, 1.0000] | yes | higher better | no change |
| batch_composite | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| chargeback | Accuracy | 0.6667 | 0.6667 | +0.00 | [0.3906, 0.8619] | [0.3906, 0.8619] | yes | higher better | no change |
| chargeback | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| chargeback | Correct escalation | 0.6667 | 0.6667 | +0.00 | [0.3906, 0.8619] | [0.3906, 0.8619] | yes | higher better | no change |
| chargeback | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| chargeback | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.2065, 1.0000] | [0.4385, 1.0000] | yes | higher better | no change |
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
| fee_variance | Over-escalation | 0.3333 | 0.5000 | +16.67 | [0.1381, 0.6094] | [0.2538, 0.7462] | yes | lower better | no change |
| fee_variance | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.6097, 1.0000] | [0.5655, 1.0000] | yes | higher better | no change |
| fee_variance | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Accuracy | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| fx_rounding | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| fx_rounding | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Over-escalation | 0.3333 | 0.5000 | +16.67 | [0.1381, 0.6094] | [0.2538, 0.7462] | yes | lower better | no change |
| fx_rounding | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.6097, 1.0000] | [0.5655, 1.0000] | yes | higher better | no change |
| fx_rounding | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| injection_memo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| injection_memo | Correct escalation | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| injection_memo | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| injection_memo | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Accuracy | 0.6667 | 0.9167 | +25.00 | [0.3906, 0.8619] | [0.6461, 0.9851] | yes | higher better | no change |
| late_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| late_settlement | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Over-escalation | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| late_settlement | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.6756, 1.0000] | [0.7412, 1.0000] | yes | higher better | no change |
| late_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| missing_settlement | Accuracy | 0.2500 | 0.8333 | +58.33 | [0.0889, 0.5323] | [0.5520, 0.9530] | no | higher better | improvement |
| missing_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| missing_settlement | Correct escalation | 0.0000 | 0.4167 | +41.67 | [0.0000, 0.2425] | [0.1933, 0.6805] | yes | higher better | no change |
| missing_settlement | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| missing_settlement | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.4385, 1.0000] | [0.5655, 1.0000] | yes | higher better | no change |
| missing_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Accuracy | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | higher better | no change |
| partial_refund | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| partial_refund | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Over-escalation | 0.2500 | 0.1667 | -8.33 | [0.0889, 0.5323] | [0.0470, 0.4480] | yes | lower better | no change |
| partial_refund | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.4385, 1.0000] | [0.6097, 1.0000] | yes | higher better | no change |
| partial_refund | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Accuracy | 0.7500 | 1.0000 | +25.00 | [0.4677, 0.9111] | [0.7575, 1.0000] | yes | higher better | no change |
| reference_typo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| reference_typo | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Over-escalation | 0.1667 | 0.0000 | -16.67 | [0.0470, 0.4480] | [0.0000, 0.2425] | yes | lower better | no change |
| reference_typo | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.7009, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| reference_typo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| unknown_deposit | Accuracy | 0.9167 | 0.5000 | -41.67 | [0.6461, 0.9851] | [0.2538, 0.7462] | yes | higher better | no change |
| unknown_deposit | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| unknown_deposit | Correct escalation | 0.9167 | 0.5000 | -41.67 | [0.6461, 0.9851] | [0.2538, 0.7462] | yes | higher better | no change |
| unknown_deposit | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| unknown_deposit | Evidence validity | 1.0000 | 1.0000 | +0.00 | [0.2065, 1.0000] | [0.6097, 1.0000] | yes | higher better | no change |
| unknown_deposit | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## Cost, latency, and steps

Descriptive only; these measurements never drive the exit code.

| Measure | A | B | Δ | Δ% |
|---|---|---|---|---|
| Cost mean (USD) | 0.003795 | 0.002298 | -0.001497 | -39.45 |
| Latency p50 (ms) | 0 | 0 | +0 | n/a |
| Latency p95 (ms) | 0 | 0 | +0 | n/a |
| Steps mean | 9.75 | 5.90 | -3.85 | — |

## Decision

**No gating regression.**

--report-only: exit suppressed (would exit 0).
