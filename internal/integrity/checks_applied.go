package integrity

import (
	"context"

	"github.com/TamasGorgics/recon-agent/internal/invariants"
)

// checkAppliedProposalTrail reuses the stable internal/invariants contract
// unchanged. It is intentionally a superset of FIN-52: an applied proposal with
// a missing ledger transaction, audit row, resolved break/case, or outbox row
// fails the run. Codes are mapped one-for-one and never renamed here.
func checkAppliedProposalTrail(ctx context.Context, q queryer) (CheckResult, error) {
	var checked int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM applied_proposals`).Scan(&checked); err != nil {
		return CheckResult{}, &checkError{code: "count_failed", err: err}
	}

	report, err := invariants.Check(ctx, q)
	if err != nil {
		return CheckResult{}, &checkError{code: "invariants_failed", err: err}
	}

	problems := make([]Problem, 0, len(report.Problems))
	for _, problem := range report.Problems {
		problems = append(problems, Problem{
			Code:   problem.Code,
			Entity: problem.Entity,
			ID:     problem.ID,
			Detail: boundedDetail(problem.Detail),
		})
	}
	return finalizeCheckResult(CheckAppliedProposalTrail, checked, 0, problems), nil
}
