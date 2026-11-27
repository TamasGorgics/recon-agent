package integrity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// checkTieout evaluates every reconciliation scope at its latest statement
// date at or before the run date. There is no source allowlist and no
// psp_a/bank_x special case: a scope with no statement is skipped, while a
// scope whose statement does not tie fails the check. One scope's computation
// error becomes a tieout_error problem and does not abort the other scopes.
func checkTieout(ctx context.Context, q queryer, opts checkOptions) (CheckResult, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM reconciliation_scopes ORDER BY id`)
	if err != nil {
		return CheckResult{}, &checkError{code: "scope_query_failed", err: err}
	}
	var scopeIDs []string
	for rows.Next() {
		var scopeID string
		if err := rows.Scan(&scopeID); err != nil {
			rows.Close()
			return CheckResult{}, &checkError{code: "scope_query_failed", err: err}
		}
		scopeIDs = append(scopeIDs, scopeID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CheckResult{}, &checkError{code: "scope_query_failed", err: err}
	}
	rows.Close()

	var checked, skipped int64
	var problems []Problem
	runDate := opts.RunDate.String()

	for index, scopeID := range scopeIDs {
		var asOfText string
		err := q.QueryRow(ctx, `
			SELECT as_of_date::text
			  FROM statement_balances
			 WHERE scope_id = $1::uuid
			   AND as_of_date <= $2::date
			 ORDER BY as_of_date DESC
			 LIMIT 1`, scopeID, runDate).Scan(&asOfText)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			skipped++
			continue
		case err != nil:
			return CheckResult{}, &checkError{code: "statement_query_failed", err: err}
		}

		asOf, err := time.ParseInLocation(dateLayout, asOfText, time.UTC)
		if err != nil {
			return CheckResult{}, &checkError{code: "statement_date_invalid", err: err}
		}
		checked++

		// tieout.Compute is allowed to fail after a SQL error, which aborts the
		// current transaction. A per-scope savepoint lets one broken scope
		// become a tieout_error while the other scopes and the final
		// row/alert still complete.
		savepoint := fmt.Sprintf("integrity_tieout_scope_%d", index+1)
		if _, err := q.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
			return CheckResult{}, &checkError{code: "savepoint_failed", err: err}
		}
		result, computeErr := tieout.Compute(ctx, q, scopeID, asOf, opts.TieoutObserver)
		if computeErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CheckResult{}, ctxErr
			}
			if _, err := q.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
				return CheckResult{}, &checkError{code: "rollback_failed", err: err}
			}
			if _, err := q.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
				return CheckResult{}, &checkError{code: "release_failed", err: err}
			}
			problems = append(problems, Problem{
				Code:   CodeTieoutError,
				Entity: "scope",
				ID:     scopeID,
				Detail: fmt.Sprintf("as_of=%s error=%s", asOfText, tieoutErrorCause(computeErr)),
			})
			continue
		}
		if _, err := q.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
			return CheckResult{}, &checkError{code: "release_failed", err: err}
		}
		if result.Unexplained.IsZero() {
			continue
		}
		problems = append(problems, Problem{
			Code:   CodeTieoutUnexplained,
			Entity: "scope",
			ID:     scopeID,
			Detail: fmt.Sprintf("as_of=%s unexplained_minor=%d ledger_minor=%d statement_minor=%d",
				asOfText, result.Unexplained.AmountMinor(), result.LedgerBalance.AmountMinor(),
				result.StatementClosing.AmountMinor()),
		})
	}

	return finalizeCheckResult(CheckTieout, checked, skipped, problems), nil
}

// tieoutErrorCause maps a Compute error to a bounded, stable detail token.
func tieoutErrorCause(err error) string {
	switch {
	case errors.Is(err, tieout.ErrNoStatement):
		return "statement_missing"
	case errors.Is(err, tieout.ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, tieout.ErrScopeNotFound):
		return "scope_not_found"
	case errors.Is(err, tieout.ErrCurrencyMismatch):
		return "currency_mismatch"
	default:
		return "compute_failed"
	}
}
