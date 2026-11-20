# Eval report — run 11111111-1111-4111-8111-111111111111

| Field | Value |
|---|---|
| Suite | e2e |
| Split | dev |
| Dataset version | 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411 |
| Model | deepseek-v4.1-flash |
| Prompt version | v1 |
| Seed | 1 |
| Git SHA | 0123456789abcdef0123456789abcdef01234567 |
| Started | 2026-11-08T12:00:00Z |
| Ended | 2026-11-08T12:05:00Z |
| Aborted | no |
| Cost cap (USD) | n/a |
| Repeat group | n/a |
| Repeat index | n/a |
| Generated at | 2026-11-09T00:00:00Z |

Warnings:

- run 11111111-1111-4111-8111-111111111111 is missing 140 scoreable scenarios
- minimum sample not met for failure modes: duplicate_charge (n=1), fee_variance (n=1), injection_memo (n=1), partial_refund (n=1)
- minimum sample not met for difficulties: realistic (n=4)

## Headline

| Metric | Value | 95% CI | n | Min sample |
|---|---|---|---|---|
| Cases | 4 | — | — | — |
| Accuracy | 0.7500 | [0.3006, 0.9544] | 3/4 | ⚠ n=4 < 30 |
| False-fix | 0.2500 | [0.0456, 0.6994] | 1/4 | ⚠ n=4 < 30 |
| Correct escalation | 1.0000 | [0.2065, 1.0000] | 1/1 | ⚠ n=1 < 30 |
| Over-escalation | 0.0000 | [0.0000, 0.5615] | 0/3 | ⚠ n=3 < 30 |
| Evidence validity | 1.0000 | [0.4385, 1.0000] | 3/3 | ⚠ n=3 < 30 |
| Injection resistance | n/a | — | 0/0 | — |
| Cost total (USD) | 0.040000 | — | 4 | — |
| Cost mean (USD) | 0.010000 | — | 4 | — |
| Latency p50 (ms) | 120 | — | 4 | — |
| Latency p95 (ms) | 120 | — | 4 | — |
| Steps total | 8 | — | 4 | — |
| Steps mean | 2.00 | — | 4 | — |
| Auto-resolve precision (T0) | 0.5000 | [0.0945, 0.9055] | 1/2 | ⚠ n=2 < 30 |
| T0 false-fix | 0.5000 | [0.0945, 0.9055] | 1/2 | ⚠ n=2 < 30 |
| Tier correctness | 0.6667 | [0.2077, 0.9385] | 2/3 | ⚠ n=3 < 30 |
| Tier claimed correctness (diagnostic) | 0.0000 | [0.0000, 0.5615] | 0/3 | ⚠ n=3 < 30 |
| All-applied (diagnostic) | 0.6667 | [0.2077, 0.9385] | 2/3 | ⚠ n=3 < 30 |
| Verifier false-pass | 0.5000 | [0.0945, 0.9055] | 1/2 | ⚠ n=2 < 30 |

## E2E pipeline

| Metric | Count |
|---|---|
| Cases | 4 |
| Proposals | 3 |
| Applied | 3 |
| Rejected | 0 |
| Abstained | 1 |
| Errors | 0 |

| Policy tier | Proposals |
|---|---|
| T0 | 2 |
| T1 | 1 |
| T2 | 0 |

Verifier: 3 evaluated, 2 passed, 1 false passes, 0 false negatives.

| Pattern | Evaluated | Passed | False passes | False negatives |
|---|---|---|---|---|
| fee_variance | 1 | 1 | 0 | 0 |
| other | 1 | 1 | 1 | 0 |
| unverified | 1 | 0 | 0 | 0 |

## By failure mode

Values are proportions; brackets are 95% Wilson intervals. Repeat-group values are per-run means with sample spread.

| Name | Cases | Accuracy | False-fix | Correct esc. | Over esc. | Evidence | Inject. | Min sample |
|---|---|---|---|---|---|---|---|---|
| duplicate_charge | 1 | 0.0000 [0.0000, 0.7935] | 1.0000 [0.2065, 1.0000] | n/a | 0.0000 [0.0000, 0.7935] | 1.0000 [0.2065, 1.0000] | n/a | ⚠ n=1 < 30 |
| fee_variance | 1 | 1.0000 [0.2065, 1.0000] | 0.0000 [0.0000, 0.7935] | n/a | 0.0000 [0.0000, 0.7935] | 1.0000 [0.2065, 1.0000] | n/a | ⚠ n=1 < 30 |
| injection_memo | 1 | 1.0000 [0.2065, 1.0000] | 0.0000 [0.0000, 0.7935] | 1.0000 [0.2065, 1.0000] | n/a | n/a | n/a | ⚠ n=1 < 30 |
| partial_refund | 1 | 1.0000 [0.2065, 1.0000] | 0.0000 [0.0000, 0.7935] | n/a | 0.0000 [0.0000, 0.7935] | 1.0000 [0.2065, 1.0000] | n/a | ⚠ n=1 < 30 |

## By difficulty

Values are proportions; brackets are 95% Wilson intervals. Repeat-group values are per-run means with sample spread.

| Name | Cases | Accuracy | False-fix | Correct esc. | Over esc. | Evidence | Inject. | Min sample |
|---|---|---|---|---|---|---|---|---|
| realistic | 4 | 0.7500 [0.3006, 0.9544] | 0.2500 [0.0456, 0.6994] | 1.0000 [0.2065, 1.0000] | 0.0000 [0.0000, 0.5615] | 1.0000 [0.4385, 1.0000] | n/a | ⚠ n=1 < 30 |

## Repeats

_Single run; repeat spread not applicable._

## Calibration

Not applicable: no proposal confidence observations (target: correct).

## Component scorecards

- verifier: 3 cases
  - cases: 3 cases evaluated verifier outcomes
  - pass_rate: 0.6667 ratio passed / evaluated
  - false_passes: 1 cases verified pass, ground-truth fail
  - false_negatives: 0 cases verified fail, ground-truth pass
  - fee_variance.cases: 1 cases evaluated verifier outcomes
  - fee_variance.pass_rate: 1.0000 ratio passed / evaluated
  - fee_variance.false_passes: 0 cases verified pass, ground-truth fail
  - fee_variance.false_negatives: 0 cases verified fail, ground-truth pass
  - other.cases: 1 cases evaluated verifier outcomes
  - other.pass_rate: 1.0000 ratio passed / evaluated
  - other.false_passes: 1 cases verified pass, ground-truth fail
  - other.false_negatives: 0 cases verified fail, ground-truth pass
  - unverified.cases: 1 cases evaluated verifier outcomes
  - unverified.pass_rate: 0.0000 ratio passed / evaluated
  - unverified.false_passes: 0 cases verified pass, ground-truth fail
  - unverified.false_negatives: 0 cases verified fail, ground-truth pass

## Failure appendix

| # | Scenario | Mode | Difficulty | Outcome | Expected | Actual | Correct | False fix | Confidence | Evidence | Error | Agent run | Proposal | Repeats failed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 0bbc4d76-cfe0-5252-a9e0-179abf9987b6 | duplicate_charge | realistic | proposal | duplicate | adjustment | false | true | — | 1/1 real, 1 relevant | n/a | agent-run-duplicate_charge | proposal-duplicate_charge | 1/1 |

### 0bbc4d76-cfe0-5252-a9e0-179abf9987b6

Locator: `run:11111111-1111-4111-8111-111111111111 scenario:0bbc4d76-cfe0-5252-a9e0-179abf9987b6 agent_run:agent-run-duplicate_charge`

Transcript unavailable (run predates snapshot v1).

