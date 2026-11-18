# Case lifecycle

This page is the operator and developer reference for the M4-10 case lifecycle:
the six case statuses, the single transition authority, proposal supersession,
late-match reconciliation, retry/expiry, waiting-info re-entry, the
missing-settlement SLA, and the audited admin unmatch.

`docs/spec.md` section 8.5 and `docs/ROADMAP.md` M4-10 are the requirements.
`internal/cases/state` implements the transition authority and
`internal/cases/lifecycle` implements the lifecycle services. The M4-10
decisions and deviations are recorded in `docs/open-questions.md` Q-95.

## Statuses

| Status | Meaning |
|---|---|
| `open` | The case is ready to be worked or leased. |
| `in_agent` | The case is leased to an agent/worker and owns the lease fields. |
| `proposal_pending` | The case has an active proposal awaiting policy/approval. |
| `waiting_info` | The case is paused waiting for a human reply or new source data. |
| `escalated` | A human decision is required after the shared attempt cap, the missing-settlement SLA, or a manual escalation. |
| `resolved` | Terminal. `resolution_type` and `resolved_at` are set; only a human reopen/unmatch may return the case to `open`. |

## Transition authority

`internal/cases/state.Transition` is the only production writer of
`cases.status`. Every write:

- runs in the caller's transaction (the authority never starts, commits, or
  rolls back one);
- locks the case with `SELECT ... FOR UPDATE` and applies an expected-version
  CAS plus exactly one version bump;
- validates the edge, actor class, reason, and resolution before writing;
- appends exactly one `case.transition` audit row with before/after status,
  version, resolution, and escalation fields.

Actors are `human:<id>` or `system:<component>`; `agent:*` is rejected. A
human is required for manual resolution, admin reopen, and unmatch. System
actors drive worker/lease progression, retry, matcher, executor, and SLA
writes. Rejections are typed (`ErrIllegalTransition`, `ErrActorNotAllowed`,
`ErrReasonRequired`, `ErrResolutionRequired`, `ErrStale`, ...). Existing
`case.attempt`, `case.reclaim`, and `case.resolve` audits are preserved; the
authority adds the `case.transition` row rather than replacing them.

A new case starts at `version = 0`, so the authority accepts
`ExpectedVersion >= 0` and rejects only negative versions. Case creation and
the CAS guard are otherwise unchanged.

## Proposal supersession

When a proposal is replaced or made moot, the active proposal
(`proposed`/`pending_approval`) is transitioned to `superseded` and exactly one
`approval.cancel` outbox row is enqueued with aggregate `approval`,
idempotency key `approval.cancel:<proposal id>`, and payload
`{"proposal_id","case_id","reason"}`. The replacement path leaves the case in
`proposal_pending`.

- Supersession is server/app-only. The agent role cannot supersede proposals,
  and a second agent proposal still hits the one-active-proposal unique index.
- `approved`/`auto_approved` proposals are not superseded by this path; the
  executor owns their outcome.
- Repeated calls are idempotent: once the proposal is superseded there is no
  active proposal to find, and the outbox key is the second guard.
- M4-10 writes the outbox row only. Cancel delivery and `external_ref`
  resolution are M4-12.

## Late match

After `match.Engine.Run` inserts a new active match, the lifecycle hook
reconciles any case whose breaks the match consumed. Consumed `open`/`in_case`
breaks become `superseded` with one `break.superseded` audit each.

- **All non-void attached breaks terminal** (`resolved`/`superseded`; `void`
  does not block): the case resolves with
  `resolution_type = superseded_by_match` and reason `late_match`. Any active
  `proposed`/`pending_approval` proposal is superseded and cancelled.
- **Partial consumption:** the active proposed/pending proposal is superseded
  and cancelled, and the case reopens to `open` with reason
  `partial_late_match` (from `escalated` too). Remaining breaks stay attached;
  a partially consumed case is never resolved by this path.
- **Approved/auto-approved proposal:** left intact. The executor's existing
  revalidation records `apply_failed(break_resolved)`; M4-10 makes no second
  case or proposal write.
- A resolved case with another resolution is not rewritten; reconciliation
  records a deduplicated `case.late_match_skipped` audit and stops.

Reconciliation is idempotent and runs in the matcher transaction when the hook
is wired; the case lock and the `approval.cancel` key serialize replays.

## Retry and expiry

`OnProposalClosed` handles proposals that become `rejected` or `expired`.
Every other proposal status is a no-op.

- The case must still be `proposal_pending`; `open`/`escalated` are idempotent
  no-ops, and other statuses write one deduplicated `case.retry_skipped` audit
  and change nothing.
- `cases.attempts` is the single shared counter for agent failures and
  rejected/expired proposals. The cap is **3**:
  - increment to 1 or 2: the case reopens to `open` with reason
    `proposal_rejected` or `proposal_expired`;
  - increment to 3 (or more): the case escalates with reason
    `attempts_exhausted`.
- The increment and transition commit in one transaction, and a repeated hook
  for the same proposal cannot increment twice.
- The approval decision path calls the hook in its own transaction. The
  `RetryStalledProposals` sweep is the repair path for decisions that
  committed without the hook; it is idempotent and never reopens a resolved or
  escalated case.

No scheduler/worker call site drives the retry sweep yet.

## Waiting-info re-entry

`ResumeWaiting` moves a `waiting_info` case back to `open`:

- `human_reply` requires a `human:<id>` actor;
- `new_ingest` requires exactly `system:ingest`.

An already-`open` case is an idempotent no-op. Any other status returns
`ErrIllegalTransition`; `resolved` and `escalated` cases are never auto-reopened.
The ingest attach path calls the same service through
`OnIngestTouchedCase`, which silently ignores non-`waiting_info` cases. The
external request-info callback transport is M4-12; it must call this service
rather than writing case status itself.

## Missing-settlement SLA

Configuration (top-level YAML block):

```yaml
cases:
  missing_settlement_sla_business_days: 5
```

- Default `5` when omitted; strict integer range `1..365`. Zero, negative,
  values above 365, non-integer scalars, and unknown `cases` keys fail startup
  with a `config:` error naming the key and range. Values are never clamped.
- The shared attempt cap remains the constant `3`; there is no
  `cases.max_attempts` key.

Classifier and anchor:

- Candidate statuses: `open`, `in_agent`, `proposal_pending`, `waiting_info`
  (`escalated`/`resolved` are excluded).
- The case must have at least one attached break, and every attached non-void
  break must be `ledger_only`. A non-void `external_only`, `amount_mismatch`,
  or `duplicate` break excludes the case. An all-void-break case is treated
  literally: it has attached breaks and the non-void predicate is vacuously
  true.
- The source comes from the attached breaks' scope. Mixed-source cases are
  never guessed at: they are excluded and receive one deduplicated
  `case.sla_skipped_mixed_source` audit.
- The anchor is the earliest `detected_at` among attached breaks (including
  void breaks), converted to the source timezone. At/after the source's FIN-41
  cutoff the clock starts on the next business day; otherwise it starts on the
  anchor date. The due date is N FIN-41 business days later, honoring the
  source's weekends, holidays, timezone, and DST via `internal/bizdays`.
- The case escalates with reason `missing_settlement_sla` only when the
  source-local date is strictly after the due date.

The sweep uses bounded `FOR UPDATE SKIP LOCKED` batches and is idempotent.
A source with no configured calendar is a silent skip (counted, no audit, no
error). No scheduler/worker tick calls the sweep yet.

## Admin unmatch

```text
recon match unmatch --id <match-uuid> --reason <text> --actor human:<id> [--json]
```

- `--id`, `--reason` (trimmed 1..500 bytes), and `--actor` are required.
  The actor must be an explicit `human:<id>`; `system:` and `agent:` are usage
  errors. There is no silent system actor.
- Exit codes: `0` success; `2` usage/validation (missing flag, bad UUID, bad
  actor/reason); `1` not found, already unmatched, `ErrAdjustmentApplied`, or
  transaction failure.
- `--json` prints exactly
  `{match_id, breaks_reopened, cases_reopened, proposals_cancelled, unmatched_at}`.
- The admin host needs only `database.app_url` (`DATABASE_APP_URL`); the command
  opens the application role and runs one transaction. There is no HTTP route,
  UI, or new auth system in M4-10.

In one transaction the service marks the match `unmatched` (the existing
trigger syncs legs), supersedes active proposals with reason `case_reopened`
and enqueues their cancels, reopens linked `in_case`/`resolved`/`superseded`
breaks to `open`, reopens linked cases through the state authority, and writes
exactly one `match.unmatch` audit. `open`/`void` breaks and already-open cases
are unchanged. A repeated call returns `ErrMatchNotActive` with no writes.

Before the first write, the service refuses with `ErrAdjustmentApplied` when a
linked `adjustment`/`write_off` proposal has a non-null `applied_proposals.ledger_txn_id`.
The message names the ledger transaction and tells the operator to reverse it
with the future `recon reverse --txn <id>` command. That command **does not
exist** in this release; the refusal must not imply otherwise.

## Boundaries

- **M4-12** owns cancel delivery, `external_ref` resolution, request-info
  callback transport, and vendor failures. M4-10 guarantees only the
  `approval.cancel` outbox row and its idempotency key.
- **M4-11** owns proposal decisions; M4-10 consumes them through the
  same-transaction hook and never writes case status from the approval service.
- **Executor** owns apply/apply_failed, including
  `apply_failed(break_resolved)`.
- **No `recon reverse` command** is registered in M4-10.
- **No HTTP unmatch route** is required or shipped.
- **No migration** is required: M4-10 uses existing tables and grants.
- **No scheduler** call site exists yet for `RetryStalledProposals` or
  `EscalateMissingSettlementSLA`.
