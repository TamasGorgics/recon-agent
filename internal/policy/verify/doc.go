// Package verify implements the deterministic, LLM-free verifiers for the
// four supported proposal patterns.
//
// # Contract
//
// The evaluators are pure: no database, clock, network, randomness, or shared
// mutable state. They never panic on malformed input; every malformed or
// unverifiable claim returns a failed Result with a closed machine reason code.
// Evaluators are safe for concurrent use and their output is independent of
// rationale text, confidence, wall-clock time, DB row order, and map iteration
// order.
//
// Result.Pattern is the dispatched pattern. Result.Details.Claimed is the
// untrusted claim, so a mismatch is explainable without pretending the claim
// was verified. Details.SchemaVersion is always 1; Reasons is sorted and
// de-duplicated and Reason equals Reasons[0] when non-empty.
//
// Service.Verify owns source loading for the DB-backed path. It reads the
// proposal, resolves the unique unresolved (open or in_case) case break whose
// canonical refs are all cited in the proposal evidence, loads the break's exact
// external record and ledger txn through the agent views, derives the booked fee
// (the single debit on a fees_expense_* account) and the signed net (debits
// minus credits) on the break scope's ledger account only, strictly decodes
// payload.lines (or the reference-typo ledger set), and calls the pure
// evaluator. It persists only proposals.verification and
// proposals.pattern_verified in the caller's transaction; a pass sets
// pattern_verified, every failure stores NULL. A break that is not unique
// returns break_unresolved and writes nothing.
//
// fx_rounding derives the base/quote pair from the unique rate-table base for
// the external's source and quote; proposal evidence refs are never trusted to
// choose a pair. It loads every rate at or before the occurrence instant and
// fails closed on zero bases (missing_rate) or more than one base
// (ambiguous_rate).
// reference_typo binds the payload's external_ref to the break's single
// external ref, loads the scope-account candidates with the external business
// value date as its window anchor, and requires a break that names ledger txns
// to propose exactly that set.
//
// Input projections in this package are the minimal rows the pure evaluators
// need. In particular:
//
//   - FeeSchedules contains every schedule row for the external record's
//     source/kind/currency. The evaluator selects the unique row whose
//     inclusive date range covers the external UTC date.
//   - FXRates contains the candidate fx_rates rows for the relevant
//     base/quote pair and source. The input has no base-currency field, so the
//     evaluator rejects a mixed pair set and selects the latest as_of <= the
//     external UTC instant, failing closed on ties. The service excludes rows
//     after the occurrence instant before calling the evaluator.
//   - Candidates contains eligible ledger transactions for reference_typo
//     (same reconciliation scope/source and currency, amount equal to the
//     signed net on the scope ledger account, not already matched to another
//     external record). The evaluator applies exact amount, currency, and
//     inclusive absolute value-date window checks and requires exactly one.
//     The window anchors on External.ValueDate when set, falling back to
//     OccurredAt for callers that predate the business-date field.
//
// # Pattern semantics
//
// fee_variance recomputes the fee from the unique fee schedule with the
// central money.Round half-even function and requires the ledger fee and the
// balanced adjustment to match. The generator's historical integer half-up
// calculation is deliberately not adopted: FIN-06 requires half-even.
//
// fx_rounding explains the external (half-even rounded) and ledger (truncated)
// target amounts with one positive integer source-minor amount under the stored
// rate. The exact interval is checked with decimal and big.Int arithmetic; the
// verifier never trusts either observed side. The residual must equal the
// balanced adjustment and stay within the per-currency cap.
//
// rounding requires a nonzero raw delta within the per-currency cap and both
// sides to agree after half-even rounding to the nearest whole major unit. The
// literal money.Round(minor, exponent) would be a no-op for integer minor
// units, so the implementation rounds the decimal major-unit representation
// (minor shifted by -exponent) to an integer. A zero or negative delta is not a
// rounding-only recharge and fails not_rounding.
//
// reference_typo requires exactly one eligible candidate whose proposed ledger
// set matches. A second candidate always fails, regardless of reference
// similarity; the reference string itself is not compared.
//
// # Adjustment lines
//
// For the three adjustment patterns the proposal lines must be balanced in the
// source currency: every line has the source currency and a non-negative
// amount, at least one debit and one credit exist, and the debit and credit
// sums both equal the expected delta. The account role/direction convention
// (which account is the ledger side) is the service loader's responsibility;
// the pure input only carries account code, direction, amount, and currency.
package verify
