# M3-08 dev cassette evidence

These cassettes are record-only evidence; they are not replayable and are not used by CI.

The dev runs were recorded live on 2026-11-10 against the committed `dev` split
(`dataset_version = 8b4e905f0fee854575e74a56bbe0475921a41abe06214e42061cb1be83e8b411`,
seed 1, 144 scoreable cases) at run git SHA
`7eded4c0f83c362177c6a8ace5353b2600ac97b3`. Per Q-68, case/break/run/proposal IDs
and scenario timestamps prevent byte-identical replay on another clone; deterministic
replay and the CI eval gate remain M6-05 and require a pinned world.

## Runs

| Label | Run ID | Model | Prompt | Cases | Cap USD | Actual USD | Date (UTC) |
|---|---|---|---|---:|---:|---:|---|
| A-v1 | `c2fd2117-bbd1-400d-af01-8bfb4ac6b439` | `deepseek-v4.1-flash` | `v1` | 144/144 | 1.50 | 0.618647 | 2026-11-10 |
| A-v2 | `91e15147-26fa-4625-84aa-1d07d3b224cf` | `deepseek-v4.1-flash` | `v2` | 144/144 | 1.50 | 0.546491 | 2026-11-10 |
| B-v1 | `2fc6b629-0545-42cb-a7c4-9c39af548b8e` | `glm-5.3-flash` | `v1` | 144/144 | 2.00 | 0.330880 | 2026-11-10 |

A-v1's actual spend includes a first attempt, run
`b53f4f3e-b564-485f-97d4-6b0bc400128a` (2026-11-10T13:38Z), which was aborted after
24/144 cases with `abort_reason: error` and spent $0.087183. Its cause was a
record-mode bug: a tool call with malformed JSON arguments could not be marshalled
into the cassette file. The fix stores unrepresentable arguments as `null` in the
cassette copy (the agent loop already repairs malformed arguments). The second
attempt, listed above, is the complete run whose evidence is sampled here and was
capped at the remaining $1.41. A-v1 cumulative spend is therefore
$0.618647 of the $1.50 combination cap. Total spend for the three dev runs is
**$1.496018**; including the model-B auto-tool smoke ($0.000028) the live-wave
total is **$1.496046** of the $10 hard cap.

Reports and comparison JSON are kept in the run artifact
(`.team/runs/mwamfpyl-m3-piece-d-prompt-model-variants-and-m3/`), not in Git.

## Sample policy

Full recordings are large (A-v1: 782 files/37 MiB, A-v2: 775 files/38 MiB,
B-v1: 563 files/21 MiB), so only a deterministic bounded sample is committed.
For each combination the source files are sorted by cassette key (the SHA-256
filename) in ascending byte order; files are copied until either 20 files or
0.5 MiB (524,288 bytes) would be exceeded, whichever comes first. The selected
sample is illustrative evidence, not a replay set, and it is not used by
`TestCommittedCassettes` or any replay path.

## Committed sample inventory

| Label | Files | Bytes | Source path |
|---|---:|---:|---|
| A-v1 | 10 | 486,930 | `A-v1/opencode/deepseek-v4.1-flash/v1/` |
| A-v2 | 11 | 481,252 | `A-v2/opencode/deepseek-v4.1-flash/v2/` |
| B-v1 | 13 | 505,375 | `B-v1/opencode/glm-5.3-flash/v1/` |
| **Total** | **34** | **1,473,557** | |

The 1 MiB per-file and 10 MiB total committed cassette budgets are enforced by
`TestCommittedEvalCassetteBudget`, together with schema 1, filename == key,
model/prompt path consistency, `stream: false`, `temperature: 0`, and empty
`redactions`.

## Dev comparisons

All three runs are complete and share the committed `dev` dataset version; their
comparison Markdown is committed under `eval/cassette-evidence/compare/`. All three
comparisons were generated with `--report-only`, so they exit 0 even when the gate
flags a regression. Two comparisons flag no gating regression; A-v1 vs B-v1 flags a
per-mode gating regression in `unknown_deposit` (see below).

- **A-v1 vs A-v2 (prompt effect):** accuracy 0.5347 → 0.5000 (−3.47 pp, overlapping
  Wilson intervals), false-fix 0 → 0, correct escalation 0.6667 → 0.5909,
  over-escalation 0.2692 → 0.1795, evidence validity 1.0 → 1.0.
- **A-v1 vs B-v1 (model effect):** accuracy 0.5347 → 0.5972 (+6.25 pp, overlapping
  intervals), false-fix 0 → 0, correct escalation 0.6667 → 0.5606,
  over-escalation 0.2692 → 0.1923, evidence validity 1.0 → 1.0. All overall rows are
  `no change`, but the per-mode gate flags **`unknown_deposit` accuracy and correct
  escalation 1.0000 → 0.5000** (12 cases per run, disjoint Wilson intervals,
  `[0.7575, 1.0000]` vs `[0.2538, 0.7462]`); with gating enabled this comparison would
  have exited 1. It is a small-sample weakness of model B in that mode rather than a
  prompt effect: the same mode for A-v2 → B-v1 is 0.9167 → 0.5000 (overlapping
  intervals, `no change`). The comparison is kept as report-only evidence and must not
  be read as a clean model-B result.
- **A-v2 vs B-v1 (cross-combination sanity):** accuracy 0.5000 → 0.5972 (+9.72 pp),
  false-fix 0 → 0, correct escalation 0.5909 → 0.5606, over-escalation
  0.1795 → 0.1923, evidence validity 1.0 → 1.0.
