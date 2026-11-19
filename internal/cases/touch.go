package cases

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CaseTouchedHook is invoked inside the CreateBreaks transaction once per case
// whose attached break was part of the creation pass (newly created or already
// present by canonical signature). It exists so M4-10's waiting-info re-entry
// can run in the same transaction that attaches ingest data to a case, without
// internal/cases importing internal/cases/lifecycle (which would create a
// cycle once lifecycle needs the state authority and cases needs lifecycle).
//
// The hook must be re-runnable with the caller's transaction: an error rolls
// back the entire break/case creation pass.
type CaseTouchedHook func(ctx context.Context, tx pgx.Tx, caseID string) error

// WithCaseTouchedHook installs the optional case-touch hook. The zero value
// (no option) leaves the hook nil and all behavior unchanged.
func WithCaseTouchedHook(hook CaseTouchedHook) ServiceOption {
	return func(s *Service) error {
		if hook == nil {
			return fmt.Errorf("%w: nil case touched hook", ErrInvalidRequest)
		}
		s.caseTouched = hook
		return nil
	}
}

// touchCasesTx calls the optional hook for every case attached to one of the
// breaks this creation pass observed. Cases are visited in ascending id order,
// matching the lifecycle lock order, so concurrent passess acquire case locks
// deterministically.
//
// Boundary: M1-14's CreateBreaks only attaches brand-new breaks to brand-new
// cases, so today the hook normally sees open cases and the lifecycle hook is a
// no-op. It is still the transactional attach point: a future path that
// attaches a break to an existing (including waiting_info) case must call
// touchCasesTx in its own transaction. A conflicting signature whose break is
// already attached is also treated as touched, because a new ingest supplying
// that break is new data touching the case.
func (s *Service) touchCasesTx(ctx context.Context, tx pgx.Tx, breaks []Break) error {
	if s.caseTouched == nil || len(breaks) == 0 {
		return nil
	}
	ids := make([]string, 0, len(breaks))
	for _, b := range breaks {
		ids = append(ids, b.ID)
	}
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT case_id::text
		   FROM case_breaks
		  WHERE break_id::text = ANY($1::text[])
		  ORDER BY case_id`, ids)
	if err != nil {
		return fmt.Errorf("cases: load touched cases: %w", err)
	}
	defer rows.Close()

	caseIDs := make([]string, 0)
	for rows.Next() {
		var caseID string
		if err := rows.Scan(&caseID); err != nil {
			return fmt.Errorf("cases: scan touched case: %w", err)
		}
		caseIDs = append(caseIDs, caseID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("cases: iterate touched cases: %w", err)
	}
	for _, caseID := range caseIDs {
		if err := s.caseTouched(ctx, tx, caseID); err != nil {
			return fmt.Errorf("cases: touched hook for case %s: %w", caseID, err)
		}
	}
	return nil
}
