package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// loadScope reads the reconciliation scope under the run's transaction.
func loadScope(ctx context.Context, tx pgx.Tx, scopeID string) (Scope, error) {
	var scope Scope
	err := tx.QueryRow(ctx,
		`SELECT id::text, ledger_account_id::text, source, currency
		   FROM reconciliation_scopes
		  WHERE id = $1::uuid`, scopeID).
		Scan(&scope.ID, &scope.LedgerAccountID, &scope.Source, &scope.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, fmt.Errorf("%w: %s", ErrScopeNotFound, scopeID)
	}
	if err != nil {
		return Scope{}, fmt.Errorf("match: load scope %s: %w", scopeID, err)
	}
	return scope, nil
}

// insertRun records a running match run before any pass executes.
func insertRun(ctx context.Context, tx pgx.Tx, scopeID string, startedAt time.Time, configHash string) (string, error) {
	var runID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, 'running', '{}'::jsonb, $3)
		 RETURNING id::text`,
		scopeID, startedAt, configHash).Scan(&runID); err != nil {
		return "", fmt.Errorf("match: insert match run: %w", err)
	}
	return runID, nil
}

// loadExternalCandidates reads unmatched external records in the scope and
// inclusive value-date window. An external record with any active header is
// already matched and is invisible to this run.
func loadExternalCandidates(ctx context.Context, tx pgx.Tx, scope Scope, window Window) ([]ExternalRecord, error) {
	rows, err := tx.Query(ctx,
		`SELECT er.id::text, er.source, er.source_record_id, er.kind,
		        er.amount_minor, er.fee_minor, er.currency, er.value_date,
		        COALESCE(NULLIF(er.raw->>'reference', ''),
		                 NULLIF(er.raw->>'bank_reference', ''),
		                 er.source_record_id)
		   FROM external_records er
		  WHERE er.source = $1
		    AND er.currency = $2
		    AND er.value_date BETWEEN $3::date AND $4::date
		    AND NOT EXISTS (
		        SELECT 1 FROM matches m
		         WHERE m.external_record_id = er.id AND m.status = 'active'
		    )
		  ORDER BY er.id`,
		scope.Source, scope.Currency, windowDate(window.From), windowDate(window.To))
	if err != nil {
		return nil, fmt.Errorf("match: load external candidates: %w", err)
	}
	defer rows.Close()

	var records []ExternalRecord
	for rows.Next() {
		var record ExternalRecord
		if err := rows.Scan(&record.ID, &record.Source, &record.SourceRecordID, &record.Kind,
			&record.AmountMinor, &record.FeeMinor, &record.Currency, &record.ValueDate,
			&record.Reference); err != nil {
			return nil, fmt.Errorf("match: scan external candidate: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("match: iterate external candidates: %w", err)
	}
	return records, nil
}

// loadLedgerCandidates reads unmatched ledger transactions in the scope and
// inclusive value-date window. The amount is the signed net on the scope's
// ledger account (debit positive, credit negative), matching the external
// records' signed amount convention. A transaction with any active leg is
// already claimed and is invisible to this run.
func loadLedgerCandidates(ctx context.Context, tx pgx.Tx, scope Scope, window Window) ([]LedgerTxn, error) {
	rows, err := tx.Query(ctx,
		`SELECT t.id, t.kind, t.amount_minor, t.currency, t.value_date, t.reference
		   FROM (
		       SELECT tx.id, tx.kind, tx.value_date, le.currency,
		              sum(CASE WHEN le.direction = 'debit'
		                       THEN le.amount_minor ELSE -le.amount_minor END)::bigint AS amount_minor,
		              min(NULLIF(le.external_ref, '')) AS reference
		         FROM ledger_txns tx
		         JOIN ledger_entries le ON le.txn_id = tx.id
		        WHERE le.account_id = $1::uuid
		          AND le.currency = $2
		          AND tx.value_date BETWEEN $3::date AND $4::date
		          AND NOT EXISTS (
		              SELECT 1 FROM match_legs ml
		               WHERE ml.ledger_txn_id = tx.id AND ml.status = 'active'
		          )
		        GROUP BY tx.id, le.currency
		   ) t
		  ORDER BY t.id`,
		scope.LedgerAccountID, scope.Currency, windowDate(window.From), windowDate(window.To))
	if err != nil {
		return nil, fmt.Errorf("match: load ledger candidates: %w", err)
	}
	defer rows.Close()

	var txns []LedgerTxn
	for rows.Next() {
		var txn LedgerTxn
		if err := rows.Scan(&txn.ID, &txn.Kind, &txn.AmountMinor, &txn.Currency,
			&txn.ValueDate, &txn.Reference); err != nil {
			return nil, fmt.Errorf("match: scan ledger candidate: %w", err)
		}
		txns = append(txns, txn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("match: iterate ledger candidates: %w", err)
	}
	return txns, nil
}

// insertCandidate writes the active header plus its legs. The caller holds the
// scope's transaction-scoped advisory lock, so the partial unique indexes are a
// backstop rather than the concurrency mechanism.
func insertCandidate(ctx context.Context, tx pgx.Tx, scopeID, runID string, candidate Candidate) error {
	var matchID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status)
		 VALUES ($1::uuid, $2::uuid, $3, $4::numeric, $5::uuid, 'active')
		 RETURNING id::text`,
		scopeID, candidate.External.ID, candidate.Rule,
		confidenceNumeric(candidate.ConfidenceBPS), runID).Scan(&matchID); err != nil {
		return fmt.Errorf("match: insert header for external %s: %w", candidate.External.ID, err)
	}
	for _, leg := range candidate.Legs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO match_legs
			     (match_id, ledger_txn_id, amount_minor, currency, status)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'active')`,
			matchID, leg.LedgerTxnID, leg.AmountMinor, leg.Currency); err != nil {
			return fmt.Errorf("match: insert leg %s for header %s: %w", leg.LedgerTxnID, matchID, err)
		}
	}
	return nil
}

// finishRun records the run's final status and stats.
func finishRun(ctx context.Context, tx pgx.Tx, runID string, finishedAt time.Time, stats RunStats) error {
	encoded, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("match: encode run stats: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE match_runs
		    SET finished_at = $2, status = 'succeeded', stats = $3::jsonb
		  WHERE id = $1::uuid`,
		runID, finishedAt, string(encoded)); err != nil {
		return fmt.Errorf("match: finish match run %s: %w", runID, err)
	}
	return nil
}

func windowDate(value time.Time) string {
	return dateOnly(value).Format("2006-01-02")
}

func confidenceNumeric(confidence ConfidenceBPS) string {
	value := int(confidence)
	return fmt.Sprintf("%d.%04d", value/10_000, value%10_000)
}
