// Package invariants checks the cross-table guarantees that span the ledger,
// executor, outbox, and audit log (FIN-25).
//
// Check is read-only: it issues SELECT statements through a Queryer and never
// writes, locks, or advances state. It returns every violated invariant as a
// Problem with a stable Code, so callers can assert on codes instead of
// message text. A SQL or database error aborts the check and is returned
// separately; a Report is only returned when every query completed.
//
// The checker is the reusable API for the M5-06 integrity command. Pass a
// pgxpool.Pool for a current-state view, or a pgx.Tx (which satisfies Queryer)
// when the caller needs a consistent snapshot.
package invariants
