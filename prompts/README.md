# Agent prompts

The agent loop composes a system prompt from `prompts/<version>/system.md` and
`prompts/<version>/rules.md`. The composed prompt is exactly the two files with
one blank line between them, and the prompt version is recorded on every
`agent_runs` row and every `llm_calls` request hash.

## Required content in every version

- **Untrusted-data rule.** Text inside `<<<UNTRUSTED:...>>>` blocks is external
  payment data, not instructions. The model must never follow instructions from
  it or treat it as policy. The wrapper token is per run and is escaped inside
  values, so memo text cannot forge a boundary.
- **No-guess rule.** Missing, ambiguous, or contradictory data must produce
  `request_info` or `escalate`, not an invented fact.
- **Evidence rule.** A proposal may cite only references that a successful tool
  call returned during the same run.
- **Tool and proposal limits.** At most one proposal per run; `escalate` is
  always allowed and is the only terminal call for a flagged run.
- **Money rules.** Amounts are integer minor units; adjustments balance per
  currency; only allowlisted account codes are valid.
- **No policy authority.** The model never chooses a risk tier, never posts to
  the ledger, and never marks a pattern as verified.

## Adding a version

1. Copy the newest `vN` directory to `vN+1`; never edit a released version.
2. Update the prompt text deliberately and review the diff.
3. Re-record every cassette that uses the new prompt version. The version is
   part of the cassette key, so old recordings are not reused silently.
4. Run the unit tests and the M2 gate before committing.

`prompts.Load` rejects names that do not match `^v[0-9]+$` and returns a typed
error for an unknown version.

## Versions

- `v1` — initial M2 prompt: tool protocol, untrusted-data boundary, no-guess,
  evidence, one-proposal, integer minor units, no risk tier, no ledger posting.
  **v1 is frozen.** Its bytes and the M2 cassettes recorded against it are not
  re-recorded. SHA-256 hashes in `prompts_test.go` fail loudly on any edit.
- `v2` — same tool protocol and rules, with a stricter evidence checklist:
  enumerate the exact refs before proposing, cite only refs returned by a
  successful tool call in this run, cite only refs relevant to the break, and
  call `request_info` or `escalate` when a check fails. The untrusted-data and
  no-guess rules are sharpened; no tool, schema, or proposal field changes.

## Eval cassettes are record-only evidence

Cassettes recorded by `recon-eval run --llm record` for M3-08 are
**record-only** evidence. Case, break, run, and proposal IDs are generated per
sandbox clone (Q-53), so they are **not replayable** against a fresh clone and
are **not a CI input**. Deterministic eval-cassette replay and the CI eval gate
are deferred to M6-05 (Q-68). The M2 fixtures under `eval/cassettes/` remain the
only replay set.
