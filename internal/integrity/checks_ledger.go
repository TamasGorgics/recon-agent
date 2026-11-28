package integrity

import (
	"context"
	"fmt"
)

// checkLedgerBalance independently verifies the FIN-10 invariant: per
// transaction and currency, debit minus credit is zero, and every transaction
// has at least one entry. The deferred ledger trigger normally prevents both;
// this check exists for restored data, disabled triggers, and future trigger
// bugs. It never writes and never weakens the trigger.
func checkLedgerBalance(ctx context.Context, q queryer) (CheckResult, error) {
	var checked int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM ledger_txns`).Scan(&checked); err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}

	var problems []Problem

	unbalanced, err := q.Query(ctx, `
		SELECT t.id::text,
		       e.currency,
		       sum(CASE WHEN e.direction = 'debit'
		                THEN e.amount_minor
		                ELSE -e.amount_minor END)::text
		  FROM ledger_txns t
		  JOIN ledger_entries e ON e.txn_id = t.id
		 GROUP BY t.id, e.currency
		HAVING sum(CASE WHEN e.direction = 'debit'
		                THEN e.amount_minor
		                ELSE -e.amount_minor END) <> 0
		 ORDER BY t.id, e.currency`)
	if err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}
	for unbalanced.Next() {
		var txnID, currency, netText string
		if err := unbalanced.Scan(&txnID, &currency, &netText); err != nil {
			unbalanced.Close()
			return CheckResult{}, &checkError{code: "query_failed", err: err}
		}
		net, err := parseInt64(netText)
		if err != nil {
			unbalanced.Close()
			return CheckResult{}, err
		}
		problems = append(problems, Problem{
			Code:   CodeLedgerTxnUnbalanced,
			Entity: "ledger_txn",
			ID:     txnID,
			Detail: fmt.Sprintf("currency=%s net_minor=%d", currency, net),
		})
	}
	if err := unbalanced.Err(); err != nil {
		unbalanced.Close()
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}
	unbalanced.Close()

	noEntries, err := q.Query(ctx, `
		SELECT t.id::text
		  FROM ledger_txns t
		 WHERE NOT EXISTS (SELECT 1 FROM ledger_entries e WHERE e.txn_id = t.id)
		 ORDER BY t.id`)
	if err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}
	defer noEntries.Close()
	for noEntries.Next() {
		var txnID string
		if err := noEntries.Scan(&txnID); err != nil {
			return CheckResult{}, &checkError{code: "query_failed", err: err}
		}
		problems = append(problems, Problem{
			Code:   CodeLedgerTxnNoEntries,
			Entity: "ledger_txn",
			ID:     txnID,
			Detail: "entries=0",
		})
	}
	if err := noEntries.Err(); err != nil {
		return CheckResult{}, &checkError{code: "query_failed", err: err}
	}

	return finalizeCheckResult(CheckLedgerBalance, checked, 0, problems), nil
}
