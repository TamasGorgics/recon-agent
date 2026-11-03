# Agent Instructions

This is a short pointer, not a copy of the roadmap. Read:

- `docs/ROADMAP.md` sections 1–2 — how to use the roadmap, stack defaults, code and test conventions, and the Definition of Done.
- `docs/spec.md` — the product specification (canonical; do not edit).

## Rules

1. Work on exactly one task ID per session. Before writing code, read that task, the spec sections it cites, and `docs/ROADMAP.md` section 2.
2. Write tests first for anything with an invariant: money, ledger, state machines, policy.
3. Never weaken or delete a test to make it pass. If a test or the spec looks wrong, stop, add the question to `docs/open-questions.md`, and ask.
4. Do not edit `docs/spec.md`. Spec problems go to `docs/open-questions.md`.
5. Do not start a task whose `Needs` are not merged.
6. Finish with the Definition of Done (`docs/ROADMAP.md` section 2.3) and report the commands you ran and their output.
