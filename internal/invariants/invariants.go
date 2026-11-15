package invariants

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Stable problem codes. They are part of the checker contract: callers and
// tests match on these strings, so renaming one is a breaking change.
const (
	// CodeLedgerTxnMissingAudit reports a ledger transaction without its
	// kind-specific ledger_txn audit entry: ledger.post for a non-reversal
	// transaction, ledger.reverse for a reversal.
	CodeLedgerTxnMissingAudit = "ledger_txn_missing_audit"
	// CodeAppliedRecordMissing reports an applied proposal without an
	// applied_proposals row.
	CodeAppliedRecordMissing = "applied_record_missing"
	// CodeAppliedRecordStatusMismatch reports an applied_proposals row whose
	// proposal is not in status applied.
	CodeAppliedRecordStatusMismatch = "applied_record_status_mismatch"
	// CodeAppliedMissingLedger reports an adjustment/write_off applied record
	// without an existing ledger transaction.
	CodeAppliedMissingLedger = "applied_missing_ledger"
	// CodeAppliedUnexpectedLedger reports a link/duplicate applied record that
	// carries a ledger transaction id.
	CodeAppliedUnexpectedLedger = "applied_unexpected_ledger"
	// CodeBreakNotResolved reports a break attached to an applied proposal
	// that is not resolved.
	CodeBreakNotResolved = "break_not_resolved"
	// CodeCaseNotResolved reports a case of an applied proposal that is not
	// resolved.
	CodeCaseNotResolved = "case_not_resolved"
	// CodeProposalAppliedOutboxMissing reports an applied proposal without its
	// proposal.applied outbox row.
	CodeProposalAppliedOutboxMissing = "proposal_applied_outbox_missing"
	// CodeProposalAppliedOutboxDead reports an applied proposal whose
	// proposal.applied outbox row is dead.
	CodeProposalAppliedOutboxDead = "proposal_applied_outbox_dead"
	// CodeExecutorAppliedAuditMissing reports an applied proposal without its
	// executor.applied audit entry.
	CodeExecutorAppliedAuditMissing = "executor_applied_audit_missing"
	// CodeTransitionAppliedAuditMissing reports an applied proposal without a
	// proposal.transition audit whose after.status is applied.
	CodeTransitionAppliedAuditMissing = "transition_applied_audit_missing"
	// CodeBreakResolutionAuditMissing reports an applied proposal whose break
	// resolve audit is missing.
	CodeBreakResolutionAuditMissing = "break_resolution_audit_missing"
	// CodeCaseResolutionAuditMissing reports an applied proposal whose case
	// resolve audit is missing.
	CodeCaseResolutionAuditMissing = "case_resolution_audit_missing"
	// CodeAppliedLedgerWithoutProposal reports a ledger transaction referenced
	// by an applied record that is not unique to, or owned by, that applied
	// proposal.
	CodeAppliedLedgerWithoutProposal = "applied_ledger_without_proposal"
)

// Queryer is the read surface Check needs. pgxpool.Pool, pgx.Conn, and pgx.Tx
// all satisfy it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Problem is one violated invariant. Code is a stable identifier from the
// Code* constants; Entity and ID identify the offending row, and Detail is
// human-readable context that callers must not parse.
type Problem struct {
	Code   string
	Entity string
	ID     string
	Detail string
}

// Report is the result of one Check. Problems is empty when every invariant
// holds.
type Report struct {
	Problems []Problem
}

// OK reports whether the check found no problems.
func (r Report) OK() bool {
	return len(r.Problems) == 0
}

// Check runs every invariant query and returns all violations. A SQL or
// database error aborts the check: the returned Report is empty and the error
// identifies the failing check.
func Check(ctx context.Context, q Queryer) (Report, error) {
	if q == nil {
		return Report{}, fmt.Errorf("invariants: nil queryer")
	}
	var problems []Problem
	for _, check := range checks {
		found, err := check(ctx, q)
		if err != nil {
			return Report{}, err
		}
		problems = append(problems, found...)
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Code != problems[j].Code {
			return problems[i].Code < problems[j].Code
		}
		if problems[i].Entity != problems[j].Entity {
			return problems[i].Entity < problems[j].Entity
		}
		if problems[i].ID != problems[j].ID {
			return problems[i].ID < problems[j].ID
		}
		return problems[i].Detail < problems[j].Detail
	})
	return Report{Problems: problems}, nil
}

// checkFunc runs one invariant query family and returns its problems.
type checkFunc func(context.Context, Queryer) ([]Problem, error)

// checks is the stable execution order of the checker.
var checks = []checkFunc{
	checkLedgerTxnMissingAudit,
	checkAppliedRecordMissing,
	checkAppliedRecordStatusMismatch,
	checkAppliedMissingLedger,
	checkAppliedUnexpectedLedger,
	checkBreakNotResolved,
	checkCaseNotResolved,
	checkProposalAppliedOutboxMissing,
	checkProposalAppliedOutboxDead,
	checkExecutorAppliedAuditMissing,
	checkTransitionAppliedAuditMissing,
	checkBreakResolutionAuditMissing,
	checkCaseResolutionAuditMissing,
	checkAppliedLedgerWithoutProposal,
}

// queryProblems runs one violation query and scans each row with scan.
func queryProblems(
	ctx context.Context,
	q Queryer,
	code, query string,
	scan func(pgx.Rows) (Problem, error),
) ([]Problem, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("invariants: %s: %w", code, err)
	}
	defer rows.Close()

	var problems []Problem
	for rows.Next() {
		problem, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("invariants: %s: scan: %w", code, err)
		}
		problems = append(problems, problem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invariants: %s: %w", code, err)
	}
	return problems, nil
}

// checkLedgerTxnMissingAudit reports every ledger transaction without the
// kind-specific audit entry that ledger.Service writes in the posting
// transaction: ledger.reverse for a kind=reversal row and ledger.post for every
// other kind. A present-but-wrong action is reported the same as a missing one.
func checkLedgerTxnMissingAudit(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT t.id::text, t.kind
		  FROM ledger_txns t
		 WHERE NOT EXISTS (
		       SELECT 1
		         FROM audit_log a
		        WHERE a.action = CASE WHEN t.kind = 'reversal'
		                              THEN 'ledger.reverse'
		                              ELSE 'ledger.post' END
		          AND a.entity_type = 'ledger_txn'
		          AND a.entity_id = t.id::text)
		 ORDER BY t.id`
	return queryProblems(ctx, q, CodeLedgerTxnMissingAudit, query,
		func(rows pgx.Rows) (Problem, error) {
			var id, kind string
			if err := rows.Scan(&id, &kind); err != nil {
				return Problem{}, err
			}
			action := "ledger.post"
			if kind == "reversal" {
				action = "ledger.reverse"
			}
			return Problem{
				Code:   CodeLedgerTxnMissingAudit,
				Entity: "ledger_txn",
				ID:     id,
				Detail: "ledger transaction has no " + action + " audit",
			}, nil
		})
}

// checkAppliedRecordMissing reports every applied proposal without its
// FIN-23 exactly-once applied record.
func checkAppliedRecordMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text
		  FROM proposals p
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM applied_proposals ap
		        WHERE ap.proposal_id = p.id)
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeAppliedRecordMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			if err := rows.Scan(&id); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeAppliedRecordMissing,
				Entity: "proposal",
				ID:     id,
				Detail: "applied proposal has no applied_proposals row",
			}, nil
		})
}

// checkAppliedRecordStatusMismatch reports every applied record whose proposal
// is not in status applied.
func checkAppliedRecordStatusMismatch(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT ap.proposal_id::text, p.status
		  FROM applied_proposals ap
		  JOIN proposals p ON p.id = ap.proposal_id
		 WHERE p.status <> 'applied'
		 ORDER BY ap.proposal_id`
	return queryProblems(ctx, q, CodeAppliedRecordStatusMismatch, query,
		func(rows pgx.Rows) (Problem, error) {
			var id, status string
			if err := rows.Scan(&id, &status); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeAppliedRecordStatusMismatch,
				Entity: "proposal",
				ID:     id,
				Detail: fmt.Sprintf("applied_proposals row belongs to proposal status %q", status),
			}, nil
		})
}

// checkAppliedMissingLedger reports adjustment/write_off applied proposals
// whose applied record has no ledger transaction id or names a missing
// transaction.
func checkAppliedMissingLedger(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text,
		       CASE WHEN ap.ledger_txn_id IS NULL THEN 'null' ELSE 'missing' END
		  FROM proposals p
		  JOIN applied_proposals ap ON ap.proposal_id = p.id
		  LEFT JOIN ledger_txns t ON t.id = ap.ledger_txn_id
		 WHERE p.status = 'applied'
		   AND p.type IN ('adjustment', 'write_off')
		   AND (ap.ledger_txn_id IS NULL OR t.id IS NULL)
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeAppliedMissingLedger, query,
		func(rows pgx.Rows) (Problem, error) {
			var id, kind string
			if err := rows.Scan(&id, &kind); err != nil {
				return Problem{}, err
			}
			detail := "adjustment/write_off applied record has a null ledger_txn_id"
			if kind == "missing" {
				detail = "adjustment/write_off applied record references a missing ledger transaction"
			}
			return Problem{
				Code:   CodeAppliedMissingLedger,
				Entity: "proposal",
				ID:     id,
				Detail: detail,
			}, nil
		})
}

// checkAppliedUnexpectedLedger reports link/duplicate applied proposals whose
// applied record carries a ledger transaction id. These proposal types
// deliberately post no ledger transaction (spec section 4, risk 4).
func checkAppliedUnexpectedLedger(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text, ap.ledger_txn_id::text
		  FROM proposals p
		  JOIN applied_proposals ap ON ap.proposal_id = p.id
		 WHERE p.status = 'applied'
		   AND p.type IN ('link', 'duplicate')
		   AND ap.ledger_txn_id IS NOT NULL
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeAppliedUnexpectedLedger, query,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, ledgerTxnID string
			if err := rows.Scan(&proposalID, &ledgerTxnID); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeAppliedUnexpectedLedger,
				Entity: "proposal",
				ID:     proposalID,
				Detail: "link/duplicate applied record references ledger transaction " + ledgerTxnID,
			}, nil
		})
}

// checkBreakNotResolved reports every break attached to an applied proposal
// that the apply did not resolve.
func checkBreakNotResolved(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text, b.id::text, b.status
		  FROM proposals p
		  JOIN case_breaks cb ON cb.case_id = p.case_id
		  JOIN breaks b ON b.id = cb.break_id
		 WHERE p.status = 'applied'
		   AND b.status <> 'resolved'
		 ORDER BY p.id, b.id`
	return queryProblems(ctx, q, CodeBreakNotResolved, query,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, breakID, status string
			if err := rows.Scan(&proposalID, &breakID, &status); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeBreakNotResolved,
				Entity: "break",
				ID:     breakID,
				Detail: fmt.Sprintf("break attached to applied proposal %s has status %q", proposalID, status),
			}, nil
		})
}

// checkCaseNotResolved reports every case of an applied proposal that the
// apply did not resolve.
func checkCaseNotResolved(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text, c.id::text, c.status
		  FROM proposals p
		  JOIN cases c ON c.id = p.case_id
		 WHERE p.status = 'applied'
		   AND c.status <> 'resolved'
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeCaseNotResolved, query,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, caseID, status string
			if err := rows.Scan(&proposalID, &caseID, &status); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeCaseNotResolved,
				Entity: "case",
				ID:     caseID,
				Detail: fmt.Sprintf("case of applied proposal %s has status %q", proposalID, status),
			}, nil
		})
}

// checkProposalAppliedOutboxMissing reports every applied proposal without the
// proposal.applied outbox row. Delivery state does not matter.
func checkProposalAppliedOutboxMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text
		  FROM proposals p
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM outbox o
		        WHERE o.aggregate_type = 'proposal'
		          AND o.aggregate_id = p.id::text
		          AND o.topic = 'proposal.applied')
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeProposalAppliedOutboxMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			if err := rows.Scan(&id); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeProposalAppliedOutboxMissing,
				Entity: "proposal",
				ID:     id,
				Detail: "applied proposal has no proposal.applied outbox row",
			}, nil
		})
}

// checkProposalAppliedOutboxDead reports every applied proposal whose
// proposal.applied outbox row is dead-lettered.
func checkProposalAppliedOutboxDead(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text
		  FROM proposals p
		  JOIN outbox o
		    ON o.aggregate_type = 'proposal'
		   AND o.aggregate_id = p.id::text
		   AND o.topic = 'proposal.applied'
		 WHERE p.status = 'applied'
		   AND o.dead
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeProposalAppliedOutboxDead, query,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			if err := rows.Scan(&id); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeProposalAppliedOutboxDead,
				Entity: "proposal",
				ID:     id,
				Detail: "proposal.applied outbox row is dead",
			}, nil
		})
}

// checkExecutorAppliedAuditMissing reports every applied proposal without its
// executor.applied summary audit.
func checkExecutorAppliedAuditMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text
		  FROM proposals p
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM audit_log a
		        WHERE a.action = 'executor.applied'
		          AND a.entity_type = 'proposal'
		          AND a.entity_id = p.id::text)
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeExecutorAppliedAuditMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			if err := rows.Scan(&id); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeExecutorAppliedAuditMissing,
				Entity: "proposal",
				ID:     id,
				Detail: "applied proposal has no executor.applied audit",
			}, nil
		})
}

// checkTransitionAppliedAuditMissing reports every applied proposal without
// the proposal.transition audit entry whose after.status is applied.
func checkTransitionAppliedAuditMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text
		  FROM proposals p
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM audit_log a
		        WHERE a.action = 'proposal.transition'
		          AND a.entity_type = 'proposal'
		          AND a.entity_id = p.id::text
		          AND a.after->>'status' = 'applied')
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeTransitionAppliedAuditMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			if err := rows.Scan(&id); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeTransitionAppliedAuditMissing,
				Entity: "proposal",
				ID:     id,
				Detail: "applied proposal has no proposal.transition audit with after.status applied",
			}, nil
		})
}

// checkBreakResolutionAuditMissing reports every break attached to an applied
// proposal without the matching break.resolve audit.
func checkBreakResolutionAuditMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text, b.id::text
		  FROM proposals p
		  JOIN case_breaks cb ON cb.case_id = p.case_id
		  JOIN breaks b ON b.id = cb.break_id
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM audit_log a
		        WHERE a.action = 'break.resolve'
		          AND a.entity_type = 'break'
		          AND a.entity_id = b.id::text
		          AND a.after->>'proposal_id' = p.id::text)
		 ORDER BY p.id, b.id`
	return queryProblems(ctx, q, CodeBreakResolutionAuditMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, breakID string
			if err := rows.Scan(&proposalID, &breakID); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeBreakResolutionAuditMissing,
				Entity: "break",
				ID:     breakID,
				Detail: "break attached to applied proposal " + proposalID + " has no break.resolve audit",
			}, nil
		})
}

// checkCaseResolutionAuditMissing reports every case of an applied proposal
// without the matching case.resolve audit.
func checkCaseResolutionAuditMissing(ctx context.Context, q Queryer) ([]Problem, error) {
	const query = `
		SELECT p.id::text, c.id::text
		  FROM proposals p
		  JOIN cases c ON c.id = p.case_id
		 WHERE p.status = 'applied'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM audit_log a
		        WHERE a.action = 'case.resolve'
		          AND a.entity_type = 'case'
		          AND a.entity_id = c.id::text
		          AND a.after->>'proposal_id' = p.id::text)
		 ORDER BY p.id`
	return queryProblems(ctx, q, CodeCaseResolutionAuditMissing, query,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, caseID string
			if err := rows.Scan(&proposalID, &caseID); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeCaseResolutionAuditMissing,
				Entity: "case",
				ID:     caseID,
				Detail: "case of applied proposal " + proposalID + " has no case.resolve audit",
			}, nil
		})
}

// checkAppliedLedgerWithoutProposal reports two violations of the applied
// ledger rule: a ledger transaction shared by more than one applied record,
// and a referenced ledger transaction that was not posted for that applied
// proposal (its idempotency key is proposal:<proposal id>). The unique
// constraint on applied_proposals.ledger_txn_id is the backstop for the first.
func checkAppliedLedgerWithoutProposal(ctx context.Context, q Queryer) ([]Problem, error) {
	var problems []Problem
	duplicateQuery := `
		SELECT t.id::text, count(*)::int
		  FROM applied_proposals ap
		  JOIN ledger_txns t ON t.id = ap.ledger_txn_id
		 GROUP BY t.id
		HAVING count(*) > 1
		 ORDER BY t.id`
	duplicates, err := queryProblems(ctx, q, CodeAppliedLedgerWithoutProposal, duplicateQuery,
		func(rows pgx.Rows) (Problem, error) {
			var id string
			var count int
			if err := rows.Scan(&id, &count); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeAppliedLedgerWithoutProposal,
				Entity: "ledger_txn",
				ID:     id,
				Detail: fmt.Sprintf("ledger transaction is referenced by %d applied records", count),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	problems = append(problems, duplicates...)

	ownerQuery := `
		SELECT ap.proposal_id::text, t.id::text, t.idempotency_key, p.status
		  FROM applied_proposals ap
		  JOIN ledger_txns t ON t.id = ap.ledger_txn_id
		  JOIN proposals p ON p.id = ap.proposal_id
		 WHERE p.status <> 'applied'
		    OR t.idempotency_key <> ('proposal:' || ap.proposal_id::text)
		 ORDER BY t.id`
	unowned, err := queryProblems(ctx, q, CodeAppliedLedgerWithoutProposal, ownerQuery,
		func(rows pgx.Rows) (Problem, error) {
			var proposalID, ledgerTxnID, key, status string
			if err := rows.Scan(&proposalID, &ledgerTxnID, &key, &status); err != nil {
				return Problem{}, err
			}
			return Problem{
				Code:   CodeAppliedLedgerWithoutProposal,
				Entity: "ledger_txn",
				ID:     ledgerTxnID,
				Detail: fmt.Sprintf("ledger transaction %q does not belong to applied proposal %s (status %q)",
					key, proposalID, status),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return append(problems, unowned...), nil
}
