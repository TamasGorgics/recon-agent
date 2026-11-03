// Package modes injects the M1 failure modes into a deterministic synthetic
// world and emits ground truth for them (spec section 10.1, part A; M1-10).
//
// # World and modes
//
// A World is built from one clean synth.Dataset. It owns the mutable ledger
// transactions, ground-truth scenarios, and parsed psp_a rows; bank_x files are
// copied through unchanged because no part-A mode touches a bank statement.
// A Mode mutates the world and appends exactly one failure scenario:
//
//	duplicate_charge    a second psp_a charge row with the same reference and amount
//	fee_variance        the psp_a fee/net differ from the fee schedule by a delta
//	fx_rounding         the external amount is the half-up conversion; the ledger
//	                    amount is the truncated conversion, so the delta is the
//	                    exact conversion residual
//	late_settlement     the psp_a record settles after the source cutoff
//	missing_settlement  the ledger charge has no psp_a record at all
//	partial_refund      the psp_a refund amount differs from the ledger refund
//
// Modes never import policy, agent, or database code. Generation stays DB-free
// (D-7). The only non-synth import that participates in the contract is
// internal/match: break signatures are match.BreakCandidate.Signature() so they
// use the same canonical encoding as M1-14 breaks. The synthetic scope ID in
// that signature is a stable UUIDv5 derived from (seed, source, currency);
// database scope UUIDs are assigned by load and are deliberately different.
//
// # HD-3 ground truth
//
// Every scenario carries expected_resolutions []string plus
// primary_resolution (HD-3). partial_refund allows exactly
// ["link","adjustment"] and sets primary_resolution to "adjustment", the path
// that books the difference. No scorer may assume a single canonical value.
//
// # Difficulty
//
// Difficulty changes only the Noise knobs in difficulty.go. Scenario selection,
// amounts, ledger transactions, signatures, resolutions, and tiers are identical
// across easy, realistic, and adversarial for the same seed and mix. realistic
// is GateDifficulty. Noise adds decoy external records after all scenarios are
// emitted and those decoys never appear in ground truth.
//
// # Determinism
//
// Application order is fixed (PartAModes ascending, then each mode's count).
// Target selection scans the clean scenarios in their deterministic order and
// skips already-claimed ledger transactions; there is no map iteration or wall
// clock in the selection path. The same seed, synth config, difficulty, and mix
// produce byte-identical manifest.json, ledger.json, ground_truth.json, and
// source files. The dataset config hash incorporates the canonical mix and the
// noise knob values, so a different mix is a different dataset_version.
//
// # Configuration errors
//
// A negative total, a negative count, an unknown mode (including M1-11 modes
// before they exist), or a count sum that differs from Total is a *ConfigError.
// A world with too few unused charge/refund rows is ErrInsufficientData.
package modes
