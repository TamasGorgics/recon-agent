//go:build integration

// covers: FIN-52
package integrity_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// TestValueConservation proves the FIN-52 equation is signed and active-only:
// a clean exact match passes, a dropped match passes, a negative/refund pair
// passes, an active-leg mismatch fails with a positive delta, and an unbooked
// tolerance difference fails rather than being exempted.
func TestValueConservation(t *testing.T) {
	t.Run("clean exact match", func(t *testing.T) {
		f := newFixture(t)
		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckValueConservation)
		if check.Status != integrity.StatusPassed || check.Checked != 1 {
			t.Fatalf("value_conservation status/checked = %q/%d, want passed/1", check.Status, check.Checked)
		}
	})

	t.Run("dropped match passes", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.db.Pool.Exec(context.Background(),
			`UPDATE matches
			    SET status = 'unmatched', unmatched_at = $2, unmatched_by = 'fixture',
			        unmatch_reason = 'integrity test'
			  WHERE external_record_id = $1::uuid AND status = 'active'`,
			f.externalID, f.clock.Now()); err != nil {
			t.Fatalf("drop match: %v", err)
		}
		result := f.mustRun(t, integrity.Options{})
		if result.Report.Status != integrity.StatusPassed {
			t.Fatalf("dropped-match run status = %q, want passed: %+v",
				result.Report.Status, result.Report.Checks)
		}
		check := checkByName(t, result.Report, integrity.CheckValueConservation)
		if check.Checked != 1 || check.ProblemCount != 0 {
			t.Fatalf("value_conservation checked/problems = %d/%d, want 1/0", check.Checked, check.ProblemCount)
		}
	})

	t.Run("negative refund pair passes", func(t *testing.T) {
		f := newFixture(t)
		externalID := f.insertExternal(t, "clean-refund-1", -500, fixtureValueDay)
		txnID := f.postLedger(t, "fixture-refund-1", ledger.KindPayout, fixtureValueDay, []ledger.Line{
			{AccountID: f.accounts["psp_clearing_usd"], Direction: ledger.Credit, AmountMinor: 500, Currency: "USD"},
			{AccountID: f.accounts["fees_expense"], Direction: ledger.Debit, AmountMinor: 500, Currency: "USD"},
		})
		f.insertMatch(t, f.scopeID, externalID, txnID, -500)
		if _, err := f.db.Pool.Exec(context.Background(),
			`UPDATE statement_balances SET closing_minor = 9500 WHERE id = $1::uuid`,
			f.statementID); err != nil {
			t.Fatalf("update statement for refund: %v", err)
		}

		result := f.mustRun(t, integrity.Options{})
		if result.Report.Status != integrity.StatusPassed {
			t.Fatalf("negative-pair run status = %q, want passed: %+v",
				result.Report.Status, result.Report.Checks)
		}
		check := checkByName(t, result.Report, integrity.CheckValueConservation)
		if check.Checked != 1 || check.ProblemCount != 0 {
			t.Fatalf("negative-pair value checked/problems = %d/%d, want 1/0",
				check.Checked, check.ProblemCount)
		}
	})

	t.Run("active leg mismatch", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.db.Pool.Exec(context.Background(),
			`UPDATE match_legs SET amount_minor = 9000
			  WHERE match_id = (SELECT id FROM matches
			                     WHERE external_record_id = $1::uuid AND status = 'active')`,
			f.externalID); err != nil {
			t.Fatalf("corrupt match leg: %v", err)
		}
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckValueConservation,
			integrity.CodeValueConservationMismatch)
		if problem.ID != "psp_a:USD" {
			t.Fatalf("value problem id = %q, want psp_a:USD", problem.ID)
		}
		if !strings.Contains(problem.Detail, "ingested_minor=10000") ||
			!strings.Contains(problem.Detail, "accounted_minor=9000") ||
			!strings.Contains(problem.Detail, "delta_minor=1000") {
			t.Fatalf("value detail = %q, want ingested 10000 / accounted 9000 / delta 1000", problem.Detail)
		}
	})

	t.Run("unbooked tolerance fails", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		if _, err := f.db.Pool.Exec(ctx,
			`UPDATE external_records SET amount_minor = 19950 WHERE id = $1::uuid`,
			f.externalID); err != nil {
			t.Fatalf("update external tolerance amount: %v", err)
		}
		if _, err := f.db.Pool.Exec(ctx,
			`UPDATE match_legs SET amount_minor = 20000
			  WHERE match_id = (SELECT id FROM matches
			                     WHERE external_record_id = $1::uuid AND status = 'active')`,
			f.externalID); err != nil {
			t.Fatalf("update tolerance leg amount: %v", err)
		}
		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckValueConservation,
			integrity.CodeValueConservationMismatch)
		// The frozen equation is delta = ingested - (matched + unmatched), so
		// an external 19,950 against a 20,000 active leg is -50. The spec's
		// prose calls the unbooked tolerance "+50"; the equation is authoritative.
		if !strings.Contains(problem.Detail, "ingested_minor=19950") ||
			!strings.Contains(problem.Detail, "accounted_minor=20000") ||
			!strings.Contains(problem.Detail, "delta_minor=-50") {
			t.Fatalf("tolerance detail = %q, want ingested 19950 / accounted 20000 / delta -50", problem.Detail)
		}
	})

	t.Run("cross-scope attribution fails", func(t *testing.T) {
		// A psp_b record matched under the psp_a scope must not net away.
		// Matched value is attributed to the match header's scope source while
		// ingested value uses the record's own source, so both keys surface a
		// signed delta instead of hiding the cross-scope value.
		f := newFixture(t)
		f.insertAccount(t, "psp_b_clearing_usd", "asset")
		externalID := f.insertExternalFromSource(t, "psp_b", "cross-scope-1", 10000, fixtureValueDay)
		txnID := f.postLedger(t, "fixture-cross-scope-1", ledger.KindCharge, fixtureValueDay, []ledger.Line{
			{AccountID: f.accounts["psp_b_clearing_usd"], Direction: ledger.Debit, AmountMinor: 10000, Currency: "USD"},
			{AccountID: f.accounts["fees_expense"], Direction: ledger.Credit, AmountMinor: 10000, Currency: "USD"},
		})
		// Deliberately use the existing psp_a scope for the psp_b record.
		f.insertMatch(t, f.scopeID, externalID, txnID, 10000)

		result := f.mustRun(t, integrity.Options{})
		check := checkByName(t, result.Report, integrity.CheckValueConservation)
		if check.Status != integrity.StatusFailed || len(check.Problems) != 2 {
			t.Fatalf("cross-scope value check = status %q problems %+v, want failed/2",
				check.Status, check.Problems)
		}
		byID := make(map[string]string, len(check.Problems))
		for _, problem := range check.Problems {
			if problem.Code != integrity.CodeValueConservationMismatch {
				t.Fatalf("cross-scope problem code = %q, want %q",
					problem.Code, integrity.CodeValueConservationMismatch)
			}
			byID[problem.ID] = problem.Detail
		}
		if detail, ok := byID["psp_a:USD"]; !ok || !strings.Contains(detail, "delta_minor=-10000") {
			t.Fatalf("psp_a cross-scope detail = %q (present=%t), want delta_minor=-10000", detail, ok)
		}
		if detail, ok := byID["psp_b:USD"]; !ok || !strings.Contains(detail, "delta_minor=10000") {
			t.Fatalf("psp_b cross-scope detail = %q (present=%t), want delta_minor=10000", detail, ok)
		}
	})

	t.Run("numeric range is a check error", func(t *testing.T) {
		// Two unmatched records each at MaxInt64 make the ingested/unmatched
		// sum exceed int64. The check must report a bounded numeric_overflow
		// check_error, never wrap or silently skip the key.
		f := newFixture(t)
		f.insertExternalFromSource(t, "psp_a", "range-1", math.MaxInt64, fixtureValueDay)
		f.insertExternalFromSource(t, "psp_a", "range-2", math.MaxInt64, fixtureValueDay)

		result := f.mustRun(t, integrity.Options{})
		problem := requireProblemCode(t, result.Report, integrity.CheckValueConservation,
			integrity.CodeCheckError)
		if problem.Detail != "numeric_overflow" {
			t.Fatalf("numeric range detail = %q, want numeric_overflow", problem.Detail)
		}
		if check := checkByName(t, result.Report, integrity.CheckLedgerBalance); check.Status != integrity.StatusPassed {
			t.Fatalf("ledger_balance status = %q, want passed (the range fixture is balanced)", check.Status)
		}
	})
}
