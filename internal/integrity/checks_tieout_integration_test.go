//go:build integration

// covers: FIN-52
// covers: FIN-54
package integrity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/integrity"
)

// TestTieout proves every scope is considered, the latest statement at or
// before the run date is used, a missing statement is a skip (not a failure),
// a non-zero unexplained amount fails, and one scope's compute error becomes a
// tieout_error while other scopes still run.
func TestTieout(t *testing.T) {
	t.Run("clean statement ties", func(t *testing.T) {
		f := newFixture(t)
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckTieout)
		if check.Status != integrity.StatusPassed || check.Checked != 1 || check.Skipped != 0 {
			t.Fatalf("tieout status/checked/skipped = %q/%d/%d, want passed/1/0",
				check.Status, check.Checked, check.Skipped)
		}
	})

	t.Run("closing plus 100 fails", func(t *testing.T) {
		f := newFixture(t)
		f.setStatementClosing(t, f.statementID, 10100)
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckTieout,
			integrity.CodeTieoutUnexplained)
		if problem.ID != f.scopeID {
			t.Fatalf("tieout problem id = %q, want scope %q", problem.ID, f.scopeID)
		}
		if !strings.Contains(problem.Detail, "as_of="+fixtureRunDate) ||
			!strings.Contains(problem.Detail, "unexplained_minor=-100") ||
			!strings.Contains(problem.Detail, "ledger_minor=10000") ||
			!strings.Contains(problem.Detail, "statement_minor=10100") {
			t.Fatalf("tieout detail = %q", problem.Detail)
		}
	})

	t.Run("no statement scope is skipped", func(t *testing.T) {
		f := newFixture(t)
		f.insertAccount(t, "psp_b_clearing_usd", "asset")
		if _, err := f.db.Pool.Exec(context.Background(),
			`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
			 VALUES ($1::uuid, 'psp_b', 'USD')`, f.accounts["psp_b_clearing_usd"]); err != nil {
			t.Fatalf("insert no-statement scope: %v", err)
		}
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckTieout)
		if check.Status != integrity.StatusPassed || check.Checked != 1 || check.Skipped != 1 {
			t.Fatalf("tieout status/checked/skipped = %q/%d/%d, want passed/1/1",
				check.Status, check.Checked, check.Skipped)
		}
	})

	t.Run("all scopes skipped is a vacuous pass", func(t *testing.T) {
		f := newFixture(t)
		f.deleteStatement(t, f.statementID)
		result := f.mustRun(t, integrity.Options{})
		if result.Report.Status != integrity.StatusPassed {
			t.Fatalf("all-skipped run status = %q, want passed: %+v",
				result.Report.Status, result.Report.Checks)
		}
		check := checkByName(t, result.Report, integrity.CheckTieout)
		if check.Status != integrity.StatusPassed || check.Checked != 0 || check.Skipped != 1 {
			t.Fatalf("all-skipped tieout status/checked/skipped = %q/%d/%d, want passed/0/1",
				check.Status, check.Checked, check.Skipped)
		}
	})

	t.Run("compute error becomes tieout_error and other scopes continue", func(t *testing.T) {
		f := newFixture(t)
		healthyScopeID := f.insertStatementScope(t, "psp_b")
		if _, err := f.db.Pool.Exec(context.Background(),
			`UPDATE accounts SET type = 'liability' WHERE id = $1::uuid`,
			f.accounts["psp_clearing_usd"]); err != nil {
			t.Fatalf("change scope account type: %v", err)
		}
		observer := &recordingTieoutObserver{}
		result := f.mustRun(t, integrity.Options{TieoutObserver: observer})
		problem := requireProblemCode(t, result.Report, integrity.CheckTieout,
			integrity.CodeTieoutError)
		if problem.ID != f.scopeID {
			t.Fatalf("tieout_error id = %q, want scope %q", problem.ID, f.scopeID)
		}
		if !strings.Contains(problem.Detail, "error=invalid_request") {
			t.Fatalf("tieout_error detail = %q, want error=invalid_request", problem.Detail)
		}
		check := checkByName(t, result.Report, integrity.CheckTieout)
		if check.Checked != 2 {
			t.Fatalf("tieout checked = %d, want 2 (the error still counts and the other scope ran)",
				check.Checked)
		}
		if check.Skipped != 0 || len(check.Problems) != 1 {
			t.Fatalf("tieout skipped/problems = %d/%+v, want 0/one tieout_error",
				check.Skipped, check.Problems)
		}
		for _, other := range check.Problems {
			if other.ID == healthyScopeID {
				t.Fatalf("the healthy scope %q reported a problem: %+v", healthyScopeID, other)
			}
		}
		if len(observer.computed) != 1 {
			t.Fatalf("healthy scope computations = %d, want 1", len(observer.computed))
		}
		if observer.computed[0].ScopeID != healthyScopeID {
			t.Fatalf("computed scope = %q, want healthy scope %q",
				observer.computed[0].ScopeID, healthyScopeID)
		}
		if len(observer.failed) != 1 || observer.failed[0] != "invalid_request" {
			t.Fatalf("tieout observer failures = %v, want [invalid_request]", observer.failed)
		}
	})

	t.Run("tolerance plus 50 fails", func(t *testing.T) {
		f := newFixture(t)
		f.setStatementClosing(t, f.statementID, 10050)
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckTieout,
			integrity.CodeTieoutUnexplained)
		if problem.ID != f.scopeID {
			t.Fatalf("tolerance tieout id = %q, want scope %q", problem.ID, f.scopeID)
		}
		// statement 10,050 - ledger 10,000 = unexplained +50; the detail is
		// signed as ledger - statement.
		if !strings.Contains(problem.Detail, "unexplained_minor=-50") ||
			!strings.Contains(problem.Detail, "ledger_minor=10000") ||
			!strings.Contains(problem.Detail, "statement_minor=10050") {
			t.Fatalf("tolerance tieout detail = %q, want unexplained_minor=-50", problem.Detail)
		}
	})
}
