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
