# Eval run comparison — 11111111-1111-4111-8111-111111111111 vs 22222222-2222-4222-8222-222222222222

A is the baseline; B is the candidate. All deltas are `B − A`.

| Field | A (baseline) | B (candidate) |
|---|---|---|
| Run ID | 11111111-1111-4111-8111-111111111111 | 22222222-2222-4222-8222-222222222222 |
| Suite | agent | agent |
| Split | dev | dev |
| Dataset version | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 |
| Model | deepseek-v4.1-flash | glm-5.3-flash |
| Prompt version | v1 | v2 |
| Seed | 1 | 1 |
| Git SHA | 0123456789abcdef0123456789abcdef01234567 | 0123456789abcdef0123456789abcdef01234567 |
| Repeat group | n/a | n/a |
| Repeat index | n/a | n/a |
| Cases | 144 | 144 |
| Proposals | 0 | 0 |
| Confidence observations | 0 | 0 |
| Complete | yes | yes |
| Missing scoreable | 0 | 0 |
| Missing modes | none | none |
| Aborted | no | no |
| Partial | no | no |

## Overall

Rates are proportions; brackets are 95% Wilson intervals. Delta is in percentage points (B − A). `no change` includes overlapping intervals even when the point delta is large.

| Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|
| Accuracy | 0.9375 | 0.9375 | +0.00 | [0.8855, 0.9668] | [0.8855, 0.9668] | yes | higher better | no change |
| False-fix | 0.0417 | 0.0417 | +0.00 | [0.0192, 0.0879] | [0.0192, 0.0879] | yes | lower better | no change |
| Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## By failure mode

| Mode | Metric | A | B | Δ (pp) | A 95% CI | B 95% CI | Overlap | Direction | Flag |
|---|---|---|---|---|---|---|---|---|---|
| ambiguous | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| ambiguous | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| ambiguous | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| ambiguous | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| ambiguous | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| ambiguous | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| batch_composite | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| batch_composite | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| batch_composite | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| batch_composite | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| batch_composite | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| batch_composite | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| chargeback | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| chargeback | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| chargeback | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| chargeback | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| chargeback | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| chargeback | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| duplicate_charge | Accuracy | 0.5000 | 1.0000 | +50.00 | [0.2538, 0.7462] | [0.7575, 1.0000] | no | higher better | improvement |
| duplicate_charge | False-fix | 0.5000 | 0.0000 | -50.00 | [0.2538, 0.7462] | [0.0000, 0.2425] | no | lower better | improvement |
| duplicate_charge | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| duplicate_charge | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| duplicate_charge | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| duplicate_charge | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fee_variance | Accuracy | 1.0000 | 0.5000 | -50.00 | [0.7575, 1.0000] | [0.2538, 0.7462] | no | higher better | regression |
| fee_variance | False-fix | 0.0000 | 0.5000 | +50.00 | [0.0000, 0.2425] | [0.2538, 0.7462] | no | lower better | regression |
| fee_variance | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fee_variance | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| fee_variance | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fee_variance | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| fx_rounding | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| fx_rounding | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| fx_rounding | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| fx_rounding | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| injection_memo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| injection_memo | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| injection_memo | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| injection_memo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| late_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| late_settlement | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| late_settlement | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| late_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| missing_settlement | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| missing_settlement | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| missing_settlement | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| missing_settlement | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| missing_settlement | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| missing_settlement | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Accuracy | 0.7500 | 0.7500 | +0.00 | [0.4677, 0.9111] | [0.4677, 0.9111] | yes | higher better | no change |
| partial_refund | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| partial_refund | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| partial_refund | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| partial_refund | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| reference_typo | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| reference_typo | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| reference_typo | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| reference_typo | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| unknown_deposit | Accuracy | 1.0000 | 1.0000 | +0.00 | [0.7575, 1.0000] | [0.7575, 1.0000] | yes | higher better | no change |
| unknown_deposit | False-fix | 0.0000 | 0.0000 | +0.00 | [0.0000, 0.2425] | [0.0000, 0.2425] | yes | lower better | no change |
| unknown_deposit | Correct escalation | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| unknown_deposit | Over-escalation | — | — | n/a | n/a | n/a | n/a | lower better | n/a |
| unknown_deposit | Evidence validity | — | — | n/a | n/a | n/a | n/a | higher better | n/a |
| unknown_deposit | Injection resistance | — | — | n/a | n/a | n/a | n/a | higher better | n/a |

## Cost, latency, and steps

Descriptive only; these measurements never drive the exit code.

| Measure | A | B | Δ | Δ% |
|---|---|---|---|---|
| Cost mean (USD) | 0.010000 | 0.020000 | 0.010000 | +100.00 |
| Latency p50 (ms) | 100 | 150 | +50 | +50.00 |
| Latency p95 (ms) | 100 | 150 | +50 | +50.00 |
| Steps mean | 2.00 | 3.00 | 1.00 | — |

## Decision

**Gating regression detected:** False-fix (mode fee_variance); Accuracy (mode fee_variance).

Exit code 1.
