// Package synth generates deterministic synthetic ledgers, PSP settlement
// files, bank statements, and ground truth for reconciliation (M1-09).
//
// # Determinism invariants
//
// The generator is a pure function of (seed, days, difficulty, config). It
// uses an explicit math/rand/v2 PCG source created from the seed; it never
// reads package-level RNG state and never reads the wall clock. All dates are
// derived from config.SynthConfig.BaseDate, all timestamps are explicit RFC
// 3339 UTC values, and every slice/map is sorted before it is rendered. Running
// Generate twice with the same inputs produces byte-identical manifest.json,
// ledger.json, ground_truth.json, psp_a/*.csv, and bank_x/*.csv.
//
// The injected clock exists for Load (audit timestamps) and is deliberately
// not consulted by Generate. Generation therefore works with no Postgres
// connection; only Load needs a database.
//
// # Logical IDs
//
// Every generated row has a stable UUIDv5 computed over a fixed package
// namespace, the seed, and a stable name:
//
//	account       -> (seed, "account", code)
//	customer      -> (seed, "customer", index)
//	merchant      -> (seed, "merchant", index)
//	fee schedule  -> (seed, "fee_schedule", dataset_version, source, kind, currency, valid_from)
//	ledger txn    -> (seed, "txn", monotonically increasing index)
//	scenario      -> (seed, "scenario", source, source_record_id)
//
// Database defaults (gen_random_uuid, now()) never appear in output files.
// Load maps generated account codes to the database's account UUIDs before it
// calls ledger.Post, so a database whose account rows pre-date the dataset is
// still safe to load.
//
// # Files
//
//	manifest.json          generator_version, seed, days, difficulty, config_hash,
//	                       dataset_version = sha256(generator_version|seed|config_hash),
//	                       and counts.
//	ledger.json            chart of accounts, fake customers and merchants, fee
//	                       schedules, reconciliation scopes, and transactions with
//	                       explicit created_at/booked_at/value_date.
//	ground_truth.json      one clean scenario per external record with
//	                       expected_resolutions, primary_resolution (HD-3),
//	                       expected_tier, amounts, accounts, ledger_txn_ids, and
//	                       [{source, source_record_id}].
//	psp_a/<batch>.csv      one psp_a settlement batch per day per currency.
//	bank_x/<date>_<ccy>.csv one bank statement per day per currency with a
//	                       continuous opening/closing balance per scope.
//
// M1-09 emits clean data only. psp_a carries charges, refunds, fees, payouts,
// and chargebacks; bank_x carries the matching payout receipts and account
// fees. Each external record is backed by exactly one ledger transaction, so
// ledger transaction count, external record count, and ground-truth link count
// agree.
package synth
