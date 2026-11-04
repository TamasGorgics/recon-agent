//go:build integration

// covers: FIN-31
package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestVerifyEmptyChain covers the empty-chain edge case and the fromSeq bounds.
//
// covers: FIN-31
func TestVerifyEmptyChain(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify empty chain: %v", err)
	}
	if !report.OK() || report.Checked != 0 || report.HeadSeq != 0 {
		t.Fatalf("empty-chain report = %+v, want checked 0 head 0 no problems", report)
	}

	if _, err := audit.Verify(ctx, d.Pool, 1); err != nil {
		t.Fatalf("Verify(fromSeq=1) on empty chain: %v", err)
	}
	if _, err := audit.Verify(ctx, d.Pool, 2); !errors.Is(err, audit.ErrFromSeqBeyondTail) {
		t.Fatalf("Verify(fromSeq=2) error = %v, want ErrFromSeqBeyondTail", err)
	}
}

// TestVerifyFromSeq covers incremental verification and fromSeq beyond the tail.
//
// covers: FIN-31
func TestVerifyFromSeq(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	all, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil || !all.OK() || all.Checked != 3 {
		t.Fatalf("Verify(0) = %+v, %v; want checked 3 and no problems", all, err)
	}

	middle, err := audit.Verify(ctx, d.Pool, 2)
	if err != nil {
		t.Fatalf("Verify(2): %v", err)
	}
	if !middle.OK() || middle.Checked != 2 {
		t.Fatalf("Verify(2) = %+v, want checked 2 and no problems", middle)
	}

	empty, err := audit.Verify(ctx, d.Pool, 4)
	if err != nil {
		t.Fatalf("Verify(4) at tail: %v", err)
	}
	if !empty.OK() || empty.Checked != 0 {
		t.Fatalf("Verify(4) = %+v, want checked 0 and no problems", empty)
	}

	if _, err := audit.Verify(ctx, d.Pool, 5); !errors.Is(err, audit.ErrFromSeqBeyondTail) {
		t.Fatalf("Verify(5) error = %v, want ErrFromSeqBeyondTail", err)
	}
}

// TestVerifyDetectsTamperedHashedColumns tampers with each hashed column in
// turn (as the superuser, with the append-only trigger disabled inside a
// transaction that is rolled back) and asserts Verify no longer returns OK.
//
// covers: FIN-31
func TestVerifyDetectsTamperedHashedColumns(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	clean, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil || !clean.OK() {
		t.Fatalf("baseline Verify = %+v, %v; want clean", clean, err)
	}

	cases := []struct {
		name string
		sql  []string
	}{
		{"action", []string{`UPDATE audit_log SET action = action || '.tampered' WHERE seq = 2`}},
		{"before", []string{`UPDATE audit_log SET before = '{"tampered":true}'::jsonb WHERE seq = 2`}},
		{"after", []string{`UPDATE audit_log SET after = '{"tampered":true}'::jsonb WHERE seq = 2`}},
		{"actor_type", []string{`UPDATE audit_log SET actor_type = 'agent' WHERE seq = 2`}},
		{"actor_id", []string{`UPDATE audit_log SET actor_id = 'tampered' WHERE seq = 2`}},
		{"entity_type", []string{`UPDATE audit_log SET entity_type = 'tampered' WHERE seq = 2`}},
		{"entity_id", []string{`UPDATE audit_log SET entity_id = 'tampered' WHERE seq = 2`}},
		{"trace_id", []string{`UPDATE audit_log SET trace_id = 'tampered' WHERE seq = 2`}},
		{"ts", []string{`UPDATE audit_log SET ts = ts + interval '1 second' WHERE seq = 2`}},
		{"prev_hash", []string{`UPDATE audit_log SET prev_hash = decode(repeat('00', 32), 'hex') WHERE seq = 2`}},
		{"row_hash", []string{`UPDATE audit_log SET row_hash = decode(repeat('11', 32), 'hex') WHERE seq = 2`}},
		{"seq", []string{
			`ALTER TABLE audit_log ALTER COLUMN seq DROP IDENTITY`,
			`UPDATE audit_log SET seq = 100 WHERE seq = 2`,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := d.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

			if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
				t.Fatalf("disable audit triggers: %v", err)
			}
			for _, stmt := range tc.sql {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatalf("tamper %s: %v", tc.name, err)
				}
			}

			report, err := audit.Verify(ctx, tx, 0)
			if err != nil {
				t.Fatalf("Verify tampered %s: %v", tc.name, err)
			}
			if report.OK() {
				t.Fatalf("Verify did not detect tampered %s: %+v", tc.name, report)
			}
			if !hasProblem(report, audit.ProblemHashMismatch, 0) {
				t.Fatalf("tampered %s reported no hash mismatch: %+v", tc.name, report.Problems)
			}
		})
	}
}

// TestVerifyDetectsDeletedMiddleRow deletes seq 2 from a three-row chain.
//
// covers: FIN-31
func TestVerifyDetectsDeletedMiddleRow(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 2`); err != nil {
		t.Fatalf("delete middle row: %v", err)
	}

	report, err := audit.Verify(ctx, tx, 0)
	if err != nil {
		t.Fatalf("Verify deleted middle row: %v", err)
	}
	if report.OK() {
		t.Fatal("Verify did not detect a deleted middle row")
	}
	if !hasAnyProblem(report, audit.ProblemGap, audit.ProblemBrokenLink) {
		t.Fatalf("deleted middle row reported no gap/broken link: %+v", report.Problems)
	}
	if !hasProblem(report, audit.ProblemBrokenLink, 3) {
		t.Fatalf("row 3 does not report a broken prev_hash link: %+v", report.Problems)
	}
}

// TestVerifyDetectsTruncatedTail deletes the tail row(s); the chain links stay
// valid, so only the mirror chain head can prove the truncation.
//
// covers: FIN-31
func TestVerifyDetectsTruncatedTail(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	t.Run("last row", func(t *testing.T) {
		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 3`); err != nil {
			t.Fatalf("truncate tail: %v", err)
		}

		report, err := audit.Verify(ctx, tx, 0)
		if err != nil {
			t.Fatalf("Verify truncated tail: %v", err)
		}
		if !hasProblem(report, audit.ProblemTruncatedTail, 3) {
			t.Fatalf("truncated tail not reported: %+v", report.Problems)
		}
	})

	t.Run("whole chain", func(t *testing.T) {
		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
			t.Fatalf("disable triggers: %v", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM audit_log`); err != nil {
			t.Fatalf("truncate chain: %v", err)
		}

		report, err := audit.Verify(ctx, tx, 0)
		if err != nil {
			t.Fatalf("Verify empty truncated chain: %v", err)
		}
		if !hasProblem(report, audit.ProblemTruncatedTail, 1) {
			t.Fatalf("fully truncated chain not reported: %+v", report.Problems)
		}
	})
}

// TestVerifyDetectsDeletedStart deletes the genesis row.
//
// covers: FIN-31
func TestVerifyDetectsDeletedStart(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 1`); err != nil {
		t.Fatalf("delete genesis: %v", err)
	}

	report, err := audit.Verify(ctx, tx, 0)
	if err != nil {
		t.Fatalf("Verify deleted start: %v", err)
	}
	if !hasProblem(report, audit.ProblemGap, 1) {
		t.Fatalf("deleted genesis not reported as a gap at seq 1: %+v", report.Problems)
	}
}

// TestVerifyDetectsHeadTamper mutates the mirror chain head and expects a head
// mismatch, so the tail anchor is itself covered by the hash chain.
//
// covers: FIN-31
func TestVerifyDetectsHeadTamper(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	for n := 1; n <= 3; n++ {
		appendCommitted(t, ctx, d.Pool, chainEntry(t, n))
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_chain_head DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable head triggers: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE audit_chain_head SET row_hash = decode(repeat('22', 32), 'hex') WHERE seq = 3`); err != nil {
		t.Fatalf("tamper head: %v", err)
	}

	report, err := audit.Verify(ctx, tx, 0)
	if err != nil {
		t.Fatalf("Verify tampered head: %v", err)
	}
	if !hasProblem(report, audit.ProblemHeadMismatch, 3) {
		t.Fatalf("tampered head not reported: %+v", report.Problems)
	}
}
