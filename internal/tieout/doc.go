// Package tieout computes an independent balance tie-out for one
// reconciliation scope and statement date (FIN-54).
//
// It deliberately uses its own SQL over ledger balances, statement balances,
// and open items. The matcher package is never imported: `matches` and
// `match_legs` are read only for set membership, so a matcher bug that affects
// balances cannot hide a discrepancy.
package tieout
