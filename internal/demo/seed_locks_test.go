package demo

import (
	"slices"
	"sort"
	"testing"
)

// TestSeedLockTablesMatchSeedWrites pins the F-3 lock contract: the seed takes
// one EXCLUSIVE lock on exactly the tables its M1/ops/quality layers write, in
// sorted order for a stable lock order. A new seed write or an accidental
// non-seed table (for example outbox) must fail this test.
func TestSeedLockTablesMatchSeedWrites(t *testing.T) {
	want := []string{
		"accounts", "agent_runs", "applied_proposals", "approvals", "approvers",
		"audit_chain_head", "audit_log", "breaks", "case_breaks", "cases",
		"demo_metadata", "eval_case_results", "eval_runs", "external_records",
		"fee_schedules", "ledger_entries", "ledger_txns", "match_legs",
		"match_runs", "matches", "proposals", "reconciliation_scopes",
		"settlement_batches", "statement_balances",
	}
	if !slices.Equal(seedLockTables, want) {
		t.Fatalf("seedLockTables = %v\nwant %v", seedLockTables, want)
	}
	if !sort.StringsAreSorted(seedLockTables) {
		t.Fatalf("seedLockTables is not sorted: %v", seedLockTables)
	}
}
