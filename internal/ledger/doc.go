// Package ledger records posted money movements and their lifecycle.
//
// # Posting
//
// Post writes one balanced ledger transaction, its entries, and an audit entry
// inside the caller's pgx.Tx. Debits must equal credits in every currency
// (FIN-10); the check runs in code before any row is written, and the M1-03
// deferred constraint trigger remains the database backstop. Accounts must be
// active and use the entry's currency (FIN-13). Adjustment transactions may
// only target the account codes in config.LedgerConfig.AdjustmentAccounts, and
// any transaction touching a configured customer-funds account returns
// ErrCustomerFundsT2 for the policy engine to map to tier T2.
//
// Every write path takes an idempotency key (FIN-12). The canonical
// request_hash covers kind, value_date, description, the reversal link, and
// every line (sorted), but deliberately not timestamps or the caller-supplied
// id. A replay with the same hash returns the original transaction with
// Replayed=true; a different hash returns ErrIdempotencyConflict. Posting uses
// INSERT ... ON CONFLICT (idempotency_key) DO NOTHING RETURNING; a loser reads
// the committed winner, so concurrent retries of one key produce one
// transaction and one audit row.
//
// # Time, ids, and periods
//
// The service never reads the wall clock. Timestamps come from the injected
// clock.Clock unless the caller supplies CreatedAt/BookedAt, and the transaction
// id is a UUIDv4 unless the caller supplies one (D-7), so the synthetic
// generator can be deterministic. Period selection is delegated to
// internal/periods: an open period containing value_date wins, a closed one
// redirects the posting to the current open period with adjusts_period_id set,
// and a date outside every period also posts in the current open period (D-2,
// FIN-56).
//
// # Reversal
//
// Reverse loads the original entries, flips each direction, and posts them as
// kind='reversal' with reversal_of set on the original's value date. Reversing
// a reversal is allowed. A transaction-scoped advisory lock keyed on the
// original id serializes concurrent reversals; the second sees the committed
// reversal and returns ErrAlreadyReversed. Reversal is available to recon_app
// even though ledger rows are immutable (FIN-11): it never updates them.
//
// # Balance
//
// Balance(account, asOf) sums entries whose transaction value_date is less than
// or equal to asOf (inclusive, D-2) and applies the account's normal balance
// sign (D-14): assets and expenses are debits minus credits; liabilities,
// equity, and revenue are credits minus debits.
package ledger
