//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
)

// TestCreateBreaksIsIdempotent pins D-5: breaks are keyed by the canonical
// match.BreakCandidate signature, an insert conflict is a no-op, and a re-run
// (including permuted refs with the same signature) creates no duplicate break
// or case.
func TestCreateBreaksIsIdempotent(t *testing.T) {
	f := newFixture(t)
	firstBatch := insertBatch(t, f.db)
	secondBatch := insertBatch(t, f.db)

	ledgerCandidate := match.BreakCandidate{
		ScopeID:      f.scopeID,
		Side:         match.SideLedgerOnly,
		LedgerTxnIDs: []string{syntheticLedgerID(), syntheticLedgerID()},
		Currency:     "USD",
		DeltaMinor:   1234,
	}
	externalRef := f.addExternal(t, "payout-idem-1", "payout", -9000, firstBatch)
	externalCandidate := externalOnlyCandidate(t, f.scopeID, "USD", externalRef, 9000)
	dupA := f.addExternal(t, "dup-idem-a", "charge", 2500, secondBatch)
	dupB := f.addExternal(t, "dup-idem-b", "charge", 2500, secondBatch)
	duplicateCandidate := match.BreakCandidate{
		ScopeID:         f.scopeID,
		Side:            match.SideDuplicate,
		ExternalRecords: []match.RecordRef{dupA, dupB},
		Currency:        "USD",
		DeltaMinor:      0,
	}

	first := f.createBreaks(t, ledgerCandidate, externalCandidate, duplicateCandidate)
	if got, want := first.CreatedBreaks, 3; got != want {
		t.Fatalf("first run created breaks = %d, want %d", got, want)
	}
	if got, want := len(first.Cases), 3; got != want {
		t.Fatalf("first run cases = %d, want %d (one batch per break)", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM breaks`), 3; got != want {
		t.Fatalf("breaks after first run = %d, want %d", got, want)
	}

	// Reverse the candidate order and permute the duplicate refs: the canonical
	// signature sorts refs, so this must still be a pure no-op.
	shuffledDuplicate := duplicateCandidate
	shuffledDuplicate.ExternalRecords = []match.RecordRef{dupB, dupA}
	second := f.createBreaks(t, shuffledDuplicate, externalCandidate, ledgerCandidate)
	if got, want := second.CreatedBreaks, 0; got != want {
		t.Fatalf("re-run created breaks = %d, want %d", got, want)
	}
	if got, want := second.ExistingBreaks, 3; got != want {
		t.Fatalf("re-run existing breaks = %d, want %d", got, want)
	}
	if got, want := len(second.Cases), 0; got != want {
		t.Fatalf("re-run cases = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM breaks`), 3; got != want {
		t.Fatalf("breaks after re-run = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM cases`), 3; got != want {
		t.Fatalf("cases after re-run = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM case_breaks`), 3; got != want {
		t.Fatalf("case_breaks after re-run = %d, want %d", got, want)
	}

	for _, candidate := range []match.BreakCandidate{ledgerCandidate, externalCandidate, duplicateCandidate} {
		signature := candidate.Signature()
		var gotRunID, gotSide string
		if err := f.db.Pool.QueryRow(context.Background(),
			`SELECT match_run_id::text, side FROM breaks WHERE signature = $1`, signature).
			Scan(&gotRunID, &gotSide); err != nil {
			t.Fatalf("break %s: %v", signature, err)
		}
		if gotRunID != f.runID {
			t.Fatalf("break %s match_run_id = %s, want %s", signature, gotRunID, f.runID)
		}
		if gotSide != string(candidate.Side) {
			t.Fatalf("break %s side = %s, want %s", signature, gotSide, candidate.Side)
		}
		if n := f.auditCount(t, signatureEntityID(t, f, signature), cases.ActionBreakCreate); n != 1 {
			t.Fatalf("break %s create audit rows = %d, want 1", signature, n)
		}
	}
	if got, want := f.auditTotal(t), 9; got != want {
		t.Fatalf("audit rows after first run = %d, want %d (break create + break in_case + case create per break)", got, want)
	}
}

func signatureEntityID(t *testing.T, f *fixture, signature string) string {
	t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM breaks WHERE signature = $1`, signature).Scan(&id); err != nil {
		t.Fatalf("break id for signature %s: %v", signature, err)
	}
	return id
}

// TestCreateBreaksGroupsSettlementBatch covers the grouping rule: breaks whose
// external refs share one settlement batch become one case; every other break
// gets its own case. case_breaks carries the FK/uniqueness invariants.
func TestCreateBreaksGroupsSettlementBatch(t *testing.T) {
	f := newFixture(t)
	compositeBatch := insertBatch(t, f.db)
	standaloneBatch := insertBatch(t, f.db)

	payA := f.addExternal(t, "composite-payout-a", "payout", -5000, compositeBatch)
	payB := f.addExternal(t, "composite-payout-b", "payout", -7000, compositeBatch)
	standalone := f.addExternal(t, "standalone-payout", "payout", -1100, standaloneBatch)

	result := f.createBreaks(t,
		externalOnlyCandidate(t, f.scopeID, "USD", payA, 5000),
		externalOnlyCandidate(t, f.scopeID, "USD", payB, 7000),
		externalOnlyCandidate(t, f.scopeID, "USD", standalone, 1100),
		match.BreakCandidate{
			ScopeID:      f.scopeID,
			Side:         match.SideLedgerOnly,
			LedgerTxnIDs: []string{syntheticLedgerID()},
			Currency:     "USD",
			DeltaMinor:   300,
		},
	)
	if got, want := len(result.Cases), 3; got != want {
		t.Fatalf("cases = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM case_breaks`), 4; got != want {
		t.Fatalf("case_breaks = %d, want %d", got, want)
	}

	var compositeCase string
	var maxBreaks int
	for _, c := range result.Cases {
		if len(c.BreakIDs) > maxBreaks {
			maxBreaks = len(c.BreakIDs)
			compositeCase = c.ID
		}
	}
	if got, want := maxBreaks, 2; got != want {
		t.Fatalf("largest case has %d breaks, want %d", got, want)
	}
	grouped := f.getCase(t, compositeCase)
	if got, want := len(grouped.BreakIDs), 2; got != want {
		t.Fatalf("composite case breaks = %d, want %d", got, want)
	}
	if got, want := grouped.ValueMinor, int64(12000); got != want {
		t.Fatalf("composite case value = %d, want %d", got, want)
	}
	if got, want := grouped.Priority, cases.PriorityForValue(12000); got != want {
		t.Fatalf("composite case priority = %d, want %d", got, want)
	}

	// A break may belong to exactly one case (UNIQUE (break_id)).
	var existingCase, existingBreak string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT case_id::text, break_id::text FROM case_breaks ORDER BY case_id LIMIT 1`).
		Scan(&existingCase, &existingBreak); err != nil {
		t.Fatalf("read case_break: %v", err)
	}
	_, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, existingCase, existingBreak, f.clock.Now())
	if err == nil {
		t.Fatal("duplicate case_breaks insert succeeded, want unique violation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate case_breaks error = %v, want SQLSTATE 23505", err)
	}

	// FKs hold: a break reference must exist.
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		existingCase, syntheticLedgerID(), f.clock.Now()); err == nil {
		t.Fatal("case_breaks insert with unknown break succeeded, want FK violation")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("unknown break error = %v, want SQLSTATE 23503", err)
	}

	if got, want := f.count(t, `SELECT count(*) FROM breaks WHERE status = 'in_case'`), 4; got != want {
		t.Fatalf("in_case breaks = %d, want %d", got, want)
	}
	for _, c := range result.Cases {
		if n := f.auditCount(t, c.ID, cases.ActionCaseCreate); n != 1 {
			t.Fatalf("case %s create audit rows = %d, want 1", c.ID, n)
		}
		testutil.AssertAudited(t, f.db.Pool, c.ID)
	}
}

// TestResolvedBreakIsNotRecreated pins D-5's no-reopen rule: the signature is
// unique forever, so a rerun after a break was resolved inserts nothing.
func TestResolvedBreakIsNotRecreated(t *testing.T) {
	f := newFixture(t)
	batch := insertBatch(t, f.db)
	ref := f.addExternal(t, "payout-resolved", "payout", -4200, batch)
	candidate := externalOnlyCandidate(t, f.scopeID, "USD", ref, 4200)
	f.createBreaks(t, candidate)

	breakID := signatureEntityID(t, f, candidate.Signature())
	caseID := f.countString(t, `SELECT case_id::text FROM case_breaks WHERE break_id = $1::uuid`, breakID)
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE breaks SET status = 'resolved' WHERE id = $1::uuid`, breakID); err != nil {
		t.Fatalf("resolve break directly: %v", err)
	}

	again := f.createBreaks(t, candidate)
	if got, want := again.CreatedBreaks, 0; got != want {
		t.Fatalf("resolved break recreated = %d, want %d", got, want)
	}
	if got, want := again.ExistingBreaks, 1; got != want {
		t.Fatalf("existing breaks = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM breaks`), 1; got != want {
		t.Fatalf("breaks = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM case_breaks WHERE break_id = $1::uuid`, breakID), 1; got != want {
		t.Fatalf("case_breaks = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM cases`), 1; got != want {
		t.Fatalf("cases = %d, want %d", got, want)
	}
	if got, want := caseID, f.countString(t, `SELECT case_id::text FROM case_breaks WHERE break_id = $1::uuid`, breakID); got != want {
		t.Fatalf("resolved break moved to case %s, want %s", want, got)
	}
}

func (f *fixture) countString(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value string
	if err := f.db.Pool.QueryRow(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}
