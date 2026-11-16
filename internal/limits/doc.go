// Package limits implements the FIN-53 auto-apply guard between a T0 policy
// decision and the executor.
//
// Check is the integration API: T1/T2 candidates pass through, an empty or
// unknown tier fails closed as invalid_tier, and a T0 candidate is auto-applied
// only when the auto_apply_enabled switch is on, the UTC-day break rate is
// below the configured anomaly threshold, and its value still fits the
// currency's daily cumulative cap. A cap hit or a break-rate spike trips the
// existing auto_apply_enabled switch (audited as system:circuit-breaker) and
// routes the candidate to T1 with a machine reason that M4-11 persists on the
// approval audit.
//
// The package performs read-only queries for its checks; it never reserves or
// persists the candidate. TripBreaker is the only write path: it locks the
// seeded switch row, updates it once when it is enabled, and appends exactly
// one switch.set audit entry in the same transaction. Already-disabled trips
// are no-ops and append no audit. Reset is deliberately not implemented here;
// the existing admin-only POST /v1/admin/switches route is the only reset path.
//
// Because Check cannot reserve, the daily cumulative total is the sum of
// already-persisted auto_approved/applied T0 proposals only. M4-11 must persist
// (auto-approve) each Check result before checking the next candidate;
// otherwise sequential checks each read the same cumulative total and can
// bypass the daily cap. A non-empty Candidate.ProposalID is validated as a
// canonical lower-case UUID and excluded from the cumulative query so a
// candidate whose row is already persisted is counted once via ValueMinor.
// M4-09 adds no reservation table or API; M4-11 owns the persist-before-next
// ordering.
package limits
