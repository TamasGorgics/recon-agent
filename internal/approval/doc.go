// Package approval routes proposed reconciliation actions through the
// deterministic verifier, the pure policy engine, and FIN-53 limits, then
// records the human-approval workflow.
//
// # Request (M4-11 wave 1a)
//
// Request loads a proposed proposal, its case currency, and the auto-apply
// switch; runs the Verifier; derives policy Facts from the stored payload;
// evaluates policy; and applies Limits to a T0 result. It then commits in one
// transaction: the policy config version, the persisted decision, expires_at =
// clock now + TTL, the state transition to auto_approved (T0) or
// pending_approval (T1/T2), a proposal.policy audit entry, and one
// approval.request outbox row for T1/T2. A proposal that is not proposed gets
// ErrAlreadyRequested and no writes. A duplicate outbox idempotency key is an
// error, never a silent success.
//
// Limits are checked before the final transaction, so two concurrent candidates
// can both read the same cumulative total; the accepted FIN-53 read race is
// recorded in Q-93. Each Request commits its own auto-approved row before the
// caller checks the next candidate, which is what enforces the daily cap.
//
// # Tiers and channels
//
// T1 requests route to ChannelSlack and T2 requests to ChannelTicket. Payloads
// carry machine codes only. A limits T1 result is mapped onto a valid policy T1
// decision: auto_apply_disabled, daily_cap_exceeded, and break_rate_spike keep
// their code, while every other reason becomes value_unknown and the exact
// limits reason stays in the proposal.policy audit. A limits error also fails
// closed to T1 value_unknown with limits_reason=limits_unavailable.
//
// # Channel boundary
//
// Channel is the approval-channel interface; FakeChannel records requests and
// cancels with deterministic external references and performs no network I/O.
// M4-11 never sends a Slack/ticket message: the approval.request outbox row is
// the hand-off, and M4-12 owns the production adapters and callback signature
// verification.
//
// # Decide (M4-11 wave 1b)
//
// Decide records one human approved/rejected decision. It validates the closed
// decision set and the mandatory source, rejects agent/system actor strings and
// non-canonical UUIDs before any database call, and resolves an active
// approver/admin row. In one transaction it locks the proposal, expires a
// pending proposal whose expires_at has passed (system:expiry, reason expired,
// ErrExpired, no approvals row), enforces four-eyes against human audit actors
// for the approver UUID or API key id on the proposal/case/agent run, inserts
// the approvals row (channel=source, external_ref NULL, signature_valid=true),
// transitions with a human:<approver UUID> actor, and appends approval.decided.
// A repeated decision by the same approver is a no-write replay; any other
// decision on an approved/rejected proposal is ErrDecisionConflict.
// DecideByAPIKey maps approvers.api_key_id to the row and then calls Decide.
//
// # HTTP decision route (M4-11 wave 2)
//
// POST /v1/proposals/{id}/decision is mounted behind the approver role (admin
// rank is allowed) and maps the authenticated API key id to DecideByAPIKey
// with source "api". The route accepts {"decision":"approved|rejected",
// "reason":...}; the decision is the only field the service consumes. The
// optional reason is trimmed and bounded to the state machine's audit limit
// but is not persisted because the frozen Decide contract has no reason
// parameter yet (Q-94). A same-approver replay returns 200 with replayed=true;
// not-pending, expired, conflict, and co-maker failures are 409.
//
// # Proposal state machine
//
// internal/approval/state owns the status set, transition table, typed errors,
// transactional audit writes, and ExpireDue. It refuses a non-finite
// expires_at with ErrNonFiniteExpiry, and ExpireDue excludes such rows so they
// cannot starve the queue.
package approval
