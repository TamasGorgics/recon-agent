// Package modes injects the M1 failure modes into a deterministic synthetic
// world and emits ground truth for them (spec section 10.1; M1-10 part A and
// M1-11 part B).
//
// # World and modes
//
// A World is built from one clean synth.Dataset. It owns the mutable ledger
// transactions, ground-truth scenarios, parsed psp_a rows, and (for
// unknown_deposit) re-rendered bank_x statements. A Mode mutates the world and
// appends its failure scenario or scenarios:
//
//	duplicate_charge    a second psp_a charge row with the same reference and amount
//	fee_variance        the psp_a fee/net differ from the fee schedule by a delta
//	fx_rounding         the external amount is the half-up conversion; the ledger
//	                    amount is the truncated conversion, so the delta is the
//	                    exact conversion residual
//	late_settlement     the psp_a record settles after the source cutoff
//	missing_settlement  the ledger charge has no psp_a record at all
//	partial_refund      the psp_a refund amount differs from the ledger refund
//	chargeback          an unexpected psp_a debit with no ledger charge
//	batch_composite     one payout covers N-1 new ledger charges; the Nth is the
//	                    missing charge, so two scenarios are emitted per application
//	reference_typo      the psp_a reference is mangled; amount and date still match
//	unknown_deposit     a bank_x credit with no ledger counterpart
//	injection_memo      a clean link whose memo carries an adversarial template
//	ambiguous           one psp_a charge with two plausible ledger charges
//
// Modes never import policy, agent, or database code. Generation stays DB-free
// (D-7). The only non-synth imports that participate in the contract are
// internal/match, whose break signatures are match.BreakCandidate.Signature(),
// and synth/injections, whose templates are data files. The synthetic scope ID
// in a signature is a stable UUIDv5 derived from (seed, source, currency);
// database scope UUIDs are assigned by load and are deliberately different.
//
// # HD-3 ground truth
//
// Every scenario carries expected_resolutions []string plus
// primary_resolution (HD-3). partial_refund allows exactly
// ["link","adjustment"] and sets primary_resolution to "adjustment", the path
// that books the difference. ambiguous allows exactly ["request_info",
// "escalate"] and never guesses a ledger transaction. batch_composite emits one
// "link" scenario for the payout covering N-1 charges and one
// ["request_info","escalate"] scenario for the missing charge. No scorer may
// assume a single canonical value.
//
// # Injection safety
//
// injection_memo memos are loaded from synth/injections/*.txt and never drive
// ground truth: the scenario is a normal link with an empty break signature, and
// changing the template set cannot change scenario IDs, amounts, or resolutions.
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
// Application order is fixed (AllModes: part A then part B, then each mode's
// count). Target selection scans the clean scenarios in their deterministic
// order and skips already-claimed ledger transactions; there is no map iteration
// or wall clock in the selection path. The same seed, synth config, difficulty,
// and mix produce byte-identical manifest.json, ledger.json, ground_truth.json,
// and source files. The dataset config hash incorporates the canonical mix and
// the noise knob values, so a different mix is a different dataset_version.
//
// # Configuration errors
//
// A negative total, a negative count, an unknown mode, or a count sum that
// differs from Total is a *ConfigError. A world with too few unused
// charge/refund rows or no injection templates is ErrInsufficientData or
// injections.ErrNoTemplates.
package modes
