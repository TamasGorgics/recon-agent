package demo

// M5-05 F-3: seed/writer serialization. The seed writes multiple tables in one
// transaction, so the presence checks cannot rely on a RepeatableRead snapshot:
// a real eval row committed after the snapshot is taken would be invisible and
// then mixed with synthetic demo evidence. Instead the seed takes an EXCLUSIVE
// table lock over every table it writes before any presence check or marker
// snapshot. EXCLUSIVE blocks writers but is compatible with ACCESS SHARE, so
// dashboards keep reading while the seed runs.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// seedLockTables is every table the M1, ops, and quality seed layers write,
// sorted for a stable lock order (one LOCK statement, deterministic order).
// The list must not include tables the seed does not write (for example
// outbox): EXCLUSIVE would block unrelated production traffic for no reason.
var seedLockTables = []string{
	"accounts",
	"agent_runs",
	"applied_proposals",
	"approvals",
	"approvers",
	"audit_chain_head",
	"audit_log",
	"breaks",
	"case_breaks",
	"cases",
	"demo_metadata",
	"eval_case_results",
	"eval_runs",
	"external_records",
	"fee_schedules",
	"ledger_entries",
	"ledger_txns",
	"match_legs",
	"match_runs",
	"matches",
	"proposals",
	"reconciliation_scopes",
	"settlement_batches",
	"statement_balances",
}

// lockSeedTables serializes the seed against every writer that can touch the
// tables it writes. It must be the first statement after lockDemoSeed and
// before any presence check or snapshot: a writer that committed before the
// lock was acquired is visible to the READ COMMITTED presence checks (the seed
// fails closed), and a writer that starts after the lock blocks until the seed
// commits, so it can neither contaminate the seed nor be adopted as demo data.
func lockSeedTables(ctx context.Context, tx pgx.Tx) error {
	names := make([]string, 0, len(seedLockTables))
	for _, table := range seedLockTables {
		names = append(names, pgx.Identifier{table}.Sanitize())
	}
	if _, err := tx.Exec(ctx,
		"LOCK TABLE "+strings.Join(names, ", ")+" IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("demo: lock seed tables: %w", err)
	}
	return nil
}
