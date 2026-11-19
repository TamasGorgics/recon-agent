// Package traceability reads the FIN-32 decision chain without writing.
//
// ByLedgerTxn and ByCase resolve one snapshot through left joins:
//
//	ledger txn -> applied_proposals -> proposal -> case -> breaks
//	-> agent run (trace_id) -> verification -> policy decision/config
//	-> approvals/approvers -> executor audit -> ledger txn/entries
//
// The caller supplies the Queryer. Both functions only issue SELECTs, so a
// caller that opens a REPEATABLE READ READ ONLY transaction gets a single
// consistent snapshot and cannot accidentally write (FIN-32, spec section 5).
//
// A missing link is data, not an error: the chain is still returned and its
// missing array names the absent link with the exact marker vocabulary in
// package constants. Only an unknown root ledger transaction or case returns
// ErrNotFound. Every SQL statement is parameterized; the reader issues a
// constant number of statements regardless of how many chains a case has (no
// per-chain N+1).
//
// Ordering is deterministic: chains by applied-proposal creation time (proposal
// id tie-break), approvals by decision time (id tie-break), ledger entries by
// posted_at then id, and missing markers by the fixed marker order.
package traceability
