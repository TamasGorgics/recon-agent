// Package approver is the deterministic, truth-blind simulated approver used
// by the end-to-end eval pipeline.
//
// # Truth boundary
//
// The approver reads only three inputs: the routed approval request, the
// proposal facts, and the observable case facts and cited records. It never
// reads eval_truth, the dataset ground truth, eval/score results, or any other
// hidden answer source. An "exact match" means the proposal agrees with the
// observable facts and the ledger/external records it cites; scoring compares
// the resulting applied effect with ground truth independently.
//
// # Decision routing
//
//   - T0 (or an already auto-approved request) returns ActionSkipT0 with
//     rule t0_auto_approved and never calls the decider.
//   - A request with no proposal returns ActionAbstain with rule no_proposal
//     and never calls the decider.
//   - T1 and T2 share the same path. Every exact-match predicate passing
//     returns ActionApprove (rule exact_match) and calls
//     approval.Service.Decide with DecisionApproved; the first predicate
//     failure returns ActionReject with a stable "<field>_mismatch" rule and
//     calls Decide with DecisionRejected.
//
// # Side effects
//
// The package performs no network I/O and writes no approval state directly.
// Decisions always go through the Decider interface, which the production
// wiring satisfies with *approval.Service. The package uses no wall clock and
// is deterministic for the same inputs. The synthetic identity and the rule
// table are fixed constants.
package approver
