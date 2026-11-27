//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

// TestAuditChain proves the full-chain verification is read-only: a clean chain
// passes, empty passes, a hash tamper fails with hash_mismatch, and a deleted
// middle row fails without the job writing an audit row or moving the head.
func TestAuditChain(t *testing.T) {
	t.Run("clean chain", func(t *testing.T) {
		f := newFixture(t)
		wantChecked := countRows(t, f.db.Pool, `SELECT count(*) FROM audit_log`)
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckAuditChain)
		if check.Status != integrity.StatusPassed || check.Checked != wantChecked {
			t.Fatalf("audit_chain status/checked = %q/%d, want passed/%d",
				check.Status, check.Checked, wantChecked)
		}
	})

	t.Run("empty chain passes", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		for _, statement := range []string{
			`ALTER TABLE audit_log DISABLE TRIGGER USER`,
			`ALTER TABLE audit_chain_head DISABLE TRIGGER USER`,
			`DELETE FROM audit_log`,
			`DELETE FROM audit_chain_head`,
			`ALTER TABLE audit_log ENABLE TRIGGER USER`,
			`ALTER TABLE audit_chain_head ENABLE TRIGGER USER`,
		} {
			if _, err := f.db.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckAuditChain)
		if check.Status != integrity.StatusPassed || check.Checked != 0 || check.ProblemCount != 0 {
			t.Fatalf("empty audit_chain status/checked/problems = %q/%d/%d, want passed/0/0",
				check.Status, check.Checked, check.ProblemCount)
		}
	})

	t.Run("hash tamper", func(t *testing.T) {
		f := newFixture(t)
		f.disableAuditTriggers(t, func(ctx context.Context, tx pgx.Tx) {
			tag, err := tx.Exec(ctx,
				`UPDATE audit_log SET row_hash = decode(repeat('ab', 32), 'hex') WHERE seq = 1`)
			if err != nil {
				t.Fatalf("tamper audit hash: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("tampered %d audit rows, want 1", tag.RowsAffected())
			}
		})
		beforeRows := countRows(t, f.db.Pool, `SELECT count(*) FROM audit_log`)
		var beforeHead int64
		if err := f.db.Pool.QueryRow(context.Background(),
			`SELECT max(seq) FROM audit_chain_head`).Scan(&beforeHead); err != nil {
			t.Fatalf("read head before run: %v", err)
		}

		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckAuditChain,
			integrity.CodeAuditChainProblem)
		if !strings.Contains(problem.Detail, "kind=hash_mismatch") {
			t.Fatalf("tamper detail = %q, want kind=hash_mismatch", problem.Detail)
		}
		assertAuditUnchangedByRun(t, f, beforeRows, beforeHead)
	})

	t.Run("middle deletion", func(t *testing.T) {
		f := newFixture(t)
		f.disableAuditTriggers(t, func(ctx context.Context, tx pgx.Tx) {
			tag, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE seq = 2`)
			if err != nil {
				t.Fatalf("delete middle audit row: %v", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("deleted %d audit rows, want 1", tag.RowsAffected())
			}
		})
		beforeRows := countRows(t, f.db.Pool, `SELECT count(*) FROM audit_log`)
		var beforeHead int64
		if err := f.db.Pool.QueryRow(context.Background(),
			`SELECT max(seq) FROM audit_chain_head`).Scan(&beforeHead); err != nil {
			t.Fatalf("read head before run: %v", err)
		}

		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckAuditChain,
			integrity.CodeAuditChainProblem)
		if !strings.Contains(problem.Detail, "kind=gap") &&
			!strings.Contains(problem.Detail, "kind=broken_link") {
			t.Fatalf("gap detail = %q, want kind=gap or kind=broken_link", problem.Detail)
		}
		assertAuditUnchangedByRun(t, f, beforeRows, beforeHead)
	})
}

func assertAuditUnchangedByRun(t *testing.T, f *fixture, rows int64, head int64) {
	t.Helper()
	if got := countRows(t, f.db.Pool, `SELECT count(*) FROM audit_log`); got != rows {
		t.Fatalf("job changed audit_log rows: before %d after %d", rows, got)
	}
	var afterHead int64
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT max(seq) FROM audit_chain_head`).Scan(&afterHead); err != nil {
		t.Fatalf("read head after run: %v", err)
	}
	if afterHead != head {
		t.Fatalf("job moved audit_chain_head: before %d after %d", head, afterHead)
	}
}
