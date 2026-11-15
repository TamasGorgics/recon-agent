// Package fault provides compile-time-gated crash injection for the executor.
//
// Failpoint names are declared in every build so call sites compile normally.
// Hit is empty unless the binary is built with the faultinject tag; in a
// faultinject build RECON_FAULT=<point>:<action> triggers the configured
// action at a matching call site.
package fault

// Executor failpoints, in apply order. A faultinject build parses
// RECON_FAULT as "<point>:<action>", where action is "exit" (os.Exit(86)) or
// "panic".
const (
	ExecutorAfterProposalLock  = "executor.after_proposal_lock"
	ExecutorAfterRevalidation  = "executor.after_revalidation"
	ExecutorAfterLedgerInsert  = "executor.after_ledger_insert"
	ExecutorAfterAppliedInsert = "executor.after_applied_insert"
	ExecutorAfterBreakResolve  = "executor.after_break_resolve"
	ExecutorAfterCaseResolve   = "executor.after_case_resolve"
	ExecutorAfterTransition    = "executor.after_transition"
	ExecutorAfterAudit         = "executor.after_audit"
	ExecutorAfterOutboxInsert  = "executor.after_outbox_insert"
	ExecutorBeforeCommit       = "executor.before_commit"
)
