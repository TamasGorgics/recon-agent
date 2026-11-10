# M3-08 dev comparisons (Markdown)

These files are the human-readable `recon-eval compare` output for the three
M3-08 dev runs. They are record-only evidence, not CI inputs.

| File | Comparison |
|---|---|
| `a-v1-vs-a-v2/compare.md` | `deepseek-v4.1-flash` prompt `v1` → `v2` |
| `a-v1-vs-b-v1/compare.md` | `deepseek-v4.1-flash` → `glm-5.3-flash`, prompt `v1` |
| `a-v2-vs-b-v1/compare.md` | `deepseek-v4.1-flash`/`v2` → `glm-5.3-flash`/`v1` |

All three comparisons were generated with `--report-only`; each therefore exited 0.
Two flagged no gating regression. A-v1 vs B-v1 flagged a **per-mode gating
regression** in `unknown_deposit` (accuracy and correct escalation 1.0000 → 0.5000
over 12 cases per run, disjoint Wilson intervals); its Decision section records
`--report-only: exit suppressed (would exit 1)`, and the comparison must not be
described as clean. The runs are complete (144/144 cases, not aborted) and share the
committed `dev` dataset version.

Machine-readable `compare.json` and full per-run `report.md`/`report.json` live in
the untracked run artifact
(`.team/runs/mwamfpyl-m3-piece-d-prompt-model-variants-and-m3/`). They are not
committed under `eval/cassette-evidence/` because `TestCommittedEvalCassetteBudget`
treats every `.json` file in that root as a cassette and validates it as schema 1;
a comparison document is not a cassette.
