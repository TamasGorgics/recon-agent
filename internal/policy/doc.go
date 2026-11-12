// Package policy evaluates approval and action policies for proposed changes.
//
// # Contract
//
// The engine is deterministic and pure: Evaluate performs no database, clock,
// or network I/O and is safe for concurrent use. NewEngine re-parses the exact
// bytes it is about to trust, rejects a LoadedConfig.Config that is not deeply
// equal to that parse (ErrConfigMismatch), and deep-copies every policy map and
// slice. Mutating the caller's Config or Options after construction therefore
// cannot change a decision or race with Evaluate.
//
// Policy input is one committed YAML document (root policy.yaml by default)
// loaded through LoadFile or LoadBytes. The raw bytes are content-addressed with
// SHA-256; EnsureVersion stores that exact document once in
// policy_config_versions and verifies the stored bytes on later EnsureVersion
// calls. Comments and whitespace are therefore content, not formatting.
//
// # Tiers and precedence
//
// Evaluate applies the fixed order from the specification:
//
//  1. Any T2 predicate fires T2: high value, write-off above its limit, cross
//     currency, customer-funds account (FIN-13), or repeated counterparty.
//     Cross-currency is exact: T2 when the proposal value currency, or any line
//     currency, differs from the case currency, or when lines span more than one
//     currency. Empty/unknown currencies differ from a known one and fail closed.
//  2. Otherwise every T0 gate must pass: allowed type, verified allowed
//     pattern, passing matching verifier, valid non-empty evidence, known
//     in-cap value, and auto_apply_enabled.
//  3. Otherwise the first failed gate (in fixed order) sets a T1 rule, with all
//     failed gates recorded as reasons.
//  4. Confidence below confidence.downgrade_below tightens exactly one tier and
//     can never loosen one.
//
// Reasons are closed machine codes. Decision.Validate enforces that the rule is
// valid for the tier and is present in reasons. Decision.JSON is the stable persisted
// shape and carries the config hash so a decision stays explainable after the
// document changes (FIN-34).
//
// # Persistence and grants
//
// PersistDecision writes only proposals.risk_tier and
// proposals.policy_decision, in the caller's transaction, and appends no audit
// row. It requires the decision's config hash to already exist in
// policy_config_versions (ErrUnknownConfig otherwise), so M4-11 must call
// EnsureVersion before it. The additive migration
// 20261112130000_m4_04_policy_decision_grant.sql grants recon_app
// column-level UPDATE on exactly those two columns. M4-11 must call
// PersistDecision and approval/state.Transition in one transaction and
// extend the single transition audit entry with the decision summary.
//
// # Consumers
//
//   - M4-05 supplies Verification{Pattern, Passed, Details} and the verified
//     pattern; the placeholder shape is frozen.
//   - M4-09 reads AutoApplyCap/ProposalTTL for cumulative limits and the
//     circuit breaker.
//   - M4-10 derives the repeated-counterparty fact and case lifecycle inputs.
//   - M4-11 computes Facts, passes config.LedgerConfig.CustomerFundsAccounts as
//     Options.CustomerFundsAccounts, persists the decision with the state
//     transition, and owns expires_at from ProposalTTL.
//   - M4-13 owns end-to-end integrity checks across these boundaries.
package policy
