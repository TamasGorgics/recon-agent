// Package match pairs source and internal records and scores candidate matches.
//
// # Run model (M1 spec D-4)
//
// A run reconciles exactly one reconciliation scope (ledger account + source +
// currency) over an inclusive value-date window. match_runs records the run;
// matches is the header table and match_legs holds one leg per claimed ledger
// transaction. This header/leg model replaces the spec's singular
// matches.ledger_txn_id so N:1 payouts are representable; the Q-08 schema delta
// is recorded in docs/open-questions.md.
//
// Runs are incremental and idempotent. The repository loads only external
// records with no active header and ledger transactions with no active leg, so
// re-running unchanged data inserts no new matches. Rows are never deleted:
// unmatching is a status change on matches, and a trigger keeps
// match_legs.status synchronized. Partial unique indexes allow at most one
// active header per external record and one active leg per ledger transaction,
// while retaining unmatched history.
//
// # Passes and order
//
// Passes implement Pass and run in the order given to Engine. M1-12 supplies
// pass 1 (exact: same reference, amount, and currency) and pass 2 (same
// reference and currency with the absolute amount delta inside the configured
// per-currency fee tolerance). Evaluate claims accepted candidates between
// passes, so pass 2 can never re-match a record or transaction claimed by pass
// 1. A proposal group is accepted only when exactly one unmatched external
// record and one unmatched ledger transaction share its key; otherwise it is
// ambiguous and left for a later pass or for breaks. This rule, plus sorting the
// snapshot by ID, makes the result independent of input order.
//
// # References and amounts
//
// The matching reference is the psp_a raw JSON "reference", the bank_x raw JSON
// "bank_reference", or the external source_record_id as a fallback. On the
// ledger side it is ledger_entries.external_ref on an entry for the scope
// account. Amounts are signed minor units: external records store the source's
// signed net, and a ledger transaction's amount is the net on the scope account
// with debit positive and credit negative. No money value is ever a float
// (FIN-01); candidate confidence is basis points and is stored as NUMERIC.
//
// # Break signatures (M1 spec D-5)
//
// BreakCandidate.Signature returns the canonical SHA-256 hex digest over scope
// ID, side, sorted ledger transaction IDs, sorted (source, source_record_id)
// pairs, currency, and delta_minor. It excludes run ID, detection time, and
// status, so re-running the matcher over unchanged data produces the same
// signature and break writers can use ON CONFLICT (signature) DO NOTHING.
// M1-13 and M1-14 consume this type; they must not re-invent the signature.
//
// # Audit and config
//
// Every run appends exactly one system:matcher audit summary (action
// "match.run") with the run's counts and config_hash. The hash is computed over
// the sorted tolerance table so a run's matching policy is reproducible.
package match
