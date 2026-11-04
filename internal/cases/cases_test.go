//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestCaseSchemaConstraints pins the migration-level invariants that the lease
// state machine relies on: the status set, non-negative counters, and lease
// field coherence.
func TestCaseSchemaConstraints(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insert := func(status string, version, attempts int, leasedBy, leaseExpires any) error {
		_, err := f.db.Pool.Exec(ctx,
			`INSERT INTO cases
			     (priority, value_minor, currency, status, version, attempts, created_at,
			      leased_by, lease_expires_at)
			 VALUES (1, 100, 'USD', $1, $2, $3, $4, $5, $6)`,
			status, version, attempts, f.clock.Now(), leasedBy, leaseExpires)
		return err
	}
	expectCheck := func(t *testing.T, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("insert error = %v, want SQLSTATE 23514 (check violation)", err)
		}
	}

	casesUnderTest := []struct {
		name         string
		status       string
		version      int
		attempts     int
		leasedBy     any
		leaseExpires any
	}{
		{"unknown status", "reviewing", 0, 0, nil, nil},
		{"negative version", "open", -1, 0, nil, nil},
		{"negative attempts", "open", 0, -1, nil, nil},
		{"owner without expiry", "in_agent", 0, 0, "worker-1", nil},
		{"expiry without owner", "open", 0, 0, nil, testNowTimestamp()},
		{"lease on non-agent status", "open", 0, 0, "worker-1", testNowTimestamp()},
	}
	for _, tc := range casesUnderTest {
		t.Run(tc.name, func(t *testing.T) {
			expectCheck(t, insert(tc.status, tc.version, tc.attempts, tc.leasedBy, tc.leaseExpires))
		})
	}

	// A coherent leased case is accepted, so the checks above are not failing
	// for an unrelated reason.
	if err := insert("in_agent", 0, 0, "worker-1", testNowTimestamp()); err != nil {
		t.Fatalf("coherent leased case rejected: %v", err)
	}
}

func testNowTimestamp() any { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }

// TestCreateCaseRejectsEmptyAndDuplicateBreak covers the two §7 case edge
// cases: a case must have at least one break, and a break belongs to exactly
// one case.
func TestCreateCaseRejectsEmptyAndDuplicateBreak(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.service.CreateCase(ctx, nil); !errors.Is(err, cases.ErrNoBreaks) {
		t.Fatalf("CreateCase(nil) error = %v, want ErrNoBreaks", err)
	}
	if got := f.count(t, `SELECT count(*) FROM cases`); got != 0 {
		t.Fatalf("cases after empty CreateCase = %d, want 0", got)
	}

	batch := insertBatch(t, f.db)
	ref := f.addExternal(t, "one-break-two-cases", "payout", -250, batch)
	candidate := externalOnlyCandidate(t, f.scopeID, "USD", ref, 250)
	breakID := f.insertBreakRow(t, candidate)

	first, err := f.service.CreateCase(ctx, []string{breakID})
	if err != nil {
		t.Fatalf("first CreateCase: %v", err)
	}
	if got, want := len(first.BreakIDs), 1; got != want {
		t.Fatalf("first case breaks = %d, want %d", got, want)
	}
	if _, err := f.service.CreateCase(ctx, []string{breakID}); !errors.Is(err, cases.ErrBreakInCase) {
		t.Fatalf("second CreateCase error = %v, want ErrBreakInCase", err)
	}
	if got, want := f.count(t, `SELECT count(*) FROM cases`), 1; got != want {
		t.Fatalf("cases = %d, want %d", got, want)
	}
	if got, want := f.count(t, `SELECT count(*) FROM case_breaks`), 1; got != want {
		t.Fatalf("case_breaks = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, first.ID, cases.ActionCaseCreate), 1; got != want {
		t.Fatalf("case create audit rows = %d, want %d", got, want)
	}
}

// TestPriorityForValue checks that priority is derived from absolute case value
// in stable, non-decreasing bands.
func TestPriorityForValue(t *testing.T) {
	values := []int64{0, 1, 9_999, 10_000, 99_999, 100_000, 999_999_999, 10_000_000}
	last := 0
	for _, value := range values {
		got := cases.PriorityForValue(value)
		if got < 1 || got > 5 {
			t.Fatalf("PriorityForValue(%d) = %d, want 1..5", value, got)
		}
		if got < last {
			t.Fatalf("PriorityForValue(%d) = %d, decreased from %d", value, got, last)
		}
		last = got
	}
	if cases.PriorityForValue(-5_000_000) != cases.PriorityForValue(5_000_000) {
		t.Fatal("priority must use absolute value, so a negative and positive equal value share a band")
	}
}

// TestBreakSideAndCurrencyComeFromCandidate keeps the stored break row faithful
// to the canonical signature input.
func TestBreakSideAndCurrencyComeFromCandidate(t *testing.T) {
	f := newFixture(t)
	batch := insertBatch(t, f.db)
	ref := f.addExternal(t, "ledger-side", "charge", 700, batch)
	candidate := externalOnlyCandidate(t, f.scopeID, "USD", ref, -700)
	f.createBreaks(t, candidate)
	var side, currency string
	var delta int64
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT side, currency, delta_minor FROM breaks WHERE signature = $1`,
		candidate.Signature()).Scan(&side, &currency, &delta); err != nil {
		t.Fatalf("read break: %v", err)
	}
	if side != string(match.SideExternalOnly) || currency != "USD" || delta != -700 {
		t.Fatalf("break = (%s, %s, %d), want (external_only, USD, -700)", side, currency, delta)
	}
}
