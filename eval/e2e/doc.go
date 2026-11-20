// Package e2e drives the post-agent end-to-end pipeline for one eval case:
// observable read model, approval request, simulated decision, executor apply,
// and tie-out.
//
// The package is deliberately truth-blind. It reads only operational tables
// (cases, case_breaks, breaks, reconciliation_scopes, proposals,
// ledger_txns/ledger_entries, external_records, applied_proposals, approvals).
// It never reads eval_truth, dataset ground truth, transcripts, raw record
// payloads, or eval/score. Counterparty is the reconciliation scope's source;
// resolution is derived from proposals.type; money is read from ledger_entries
// joined to the scope clearing account and from integer minor units only.
//
// Pipeline order is frozen by the M4-14 spec:
//
//	no proposal -> abstain
//	proposed -> approval.Request -> (T0 auto_approved | T1/T2 pending)
//	pending -> eval/approver.Act -> approve | reject (abstain is an error)
//	approved/auto_approved -> executor.Apply
//	applied -> reload ledger txn
//	terminal -> no apply
//	then tieout.Compute when a scope and as-of are supplied
//
// Any error from Request, Act, Apply, or Compute fails closed: the partial
// eval-e2e/v1 result is returned with Failed=true and the returned error wraps
// ErrE2EFailed. Errors are bounded and never include payloads, memos, DSNs, or
// raw record contents.
//
// The package has no network code and no channel adapters. Approval outbox
// rows are written by the real approval service but never delivered here.
// Clocks and the synthetic approver identity come from the caller; the pipeline
// itself never reads the wall clock.
package e2e
