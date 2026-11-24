//go:build integration

// covers: FIN-42
package demo_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

type opsRowCounts struct {
	AgentRuns       int64
	Approvers       int64
	Proposals       int64
	Approvals       int64
	Applied         int64
	Cases           int64
	Breaks          int64
	ExternalRecords int64
	Batches         int64
	LedgerTxns      int64
	MatchRuns       int64
	Matches         int64
}

func snapshotOpsCounts(t *testing.T, d *testdb.DB) opsRowCounts {
	t.Helper()
	var counts opsRowCounts
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM agent_runs),
		        (SELECT count(*) FROM approvers),
		        (SELECT count(*) FROM proposals),
		        (SELECT count(*) FROM approvals),
		        (SELECT count(*) FROM applied_proposals),
		        (SELECT count(*) FROM cases),
		        (SELECT count(*) FROM breaks),
		        (SELECT count(*) FROM external_records),
		        (SELECT count(*) FROM settlement_batches),
		        (SELECT count(*) FROM ledger_txns),
		        (SELECT count(*) FROM match_runs),
		        (SELECT count(*) FROM matches)`).
		Scan(&counts.AgentRuns, &counts.Approvers, &counts.Proposals, &counts.Approvals,
			&counts.Applied, &counts.Cases, &counts.Breaks, &counts.ExternalRecords,
			&counts.Batches, &counts.LedgerTxns, &counts.MatchRuns, &counts.Matches); err != nil {
		t.Fatalf("snapshot ops counts: %v", err)
	}
	return counts
}

// TestM504DemoOpsFailClosed is T6: a second run without --reset fails with the
// documented message before the M1 pipeline, leaves every table unchanged, and
// a documented reset+rerun restores the same row counts.
func TestM504DemoOpsFailClosed(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	opts := demo.Options{
		DBURL:      d.DSN,
		Seed:       1,
		Days:       14,
		Difficulty: synth.DifficultyRealistic,
	}

	first, err := demo.RunOps(ctx, opts)
	if err != nil {
		t.Fatalf("first demo ops run: %v", err)
	}
	if first.AgentRuns != 8 || first.Proposals != 8 || first.Approvals != 4 || first.Applied != 5 {
		t.Fatalf("unexpected ops summary: %+v", first)
	}
	before := snapshotOpsCounts(t, d)

	if _, err := demo.RunOps(ctx, opts); !errors.Is(err, demo.ErrOpsDataPresent) {
		t.Fatalf("second demo ops run error = %v, want ErrOpsDataPresent", err)
	}
	afterRefusal := snapshotOpsCounts(t, d)
	if !reflect.DeepEqual(before, afterRefusal) {
		t.Fatalf("refused run changed counts:\nbefore=%+v\nafter=%+v", before, afterRefusal)
	}

	// The documented recovery path is the dev-only reset. It must rebuild the
	// M5-04 views and produce identical row counts from the same seed.
	if err := demo.Reset(ctx, d.DSN); err != nil {
		t.Fatalf("reset before rerun: %v", err)
	}
	if _, err := demo.RunOps(ctx, opts); err != nil {
		t.Fatalf("demo ops after reset: %v", err)
	}
	afterReset := snapshotOpsCounts(t, d)
	if !reflect.DeepEqual(before, afterReset) {
		t.Fatalf("reset+rerun counts differ:\nfirst=%+v\nafter=%+v", before, afterReset)
	}
}
