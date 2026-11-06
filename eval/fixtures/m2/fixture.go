package m2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

const (
	scopeID       = "90000000-0000-4000-8000-000000000001"
	matchRunID    = "90000000-0000-4000-8000-000000000002"
	batchID       = "90000000-0000-4000-8000-000000000003"
	statementID   = "90000000-0000-4000-8000-000000000004"
	feeScheduleID = "90000000-0000-4000-8000-000000000005"
)

// ErrAlreadyLoaded means the fixture ids already exist. Ledger rows are
// immutable, so a fixture load never deletes and reloads; callers use a fresh
// database clone.
var ErrAlreadyLoaded = errors.New("m2 fixture: already loaded; use a fresh database clone")

// Load inserts the fixed three-scenario fixture in one transaction. It must run
// against a freshly migrated database and refuses an existing fixture instead
// of overwriting immutable ledger rows.
func Load(ctx context.Context, admin db.Beginner) error {
	return db.WithTx(ctx, admin, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		caseIDs := make([]string, 0, len(world.Cases))
		for _, c := range world.Cases {
			caseIDs = append(caseIDs, c.ID)
		}
		var existing int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM cases WHERE id = ANY($1::uuid[])`, caseIDs).Scan(&existing); err != nil {
			return fmt.Errorf("m2 fixture: check existing cases: %w", err)
		}
		if existing > 0 {
			return ErrAlreadyLoaded
		}

		for _, account := range world.Accounts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO accounts (id, code, name, type, currency, status)
				 VALUES ($1::uuid, $2, $3, $4, $5, $6)`,
				account.ID, account.Code, account.Name, account.Type,
				account.Currency, account.Status); err != nil {
				return fmt.Errorf("m2 fixture: insert account %s: %w", account.Code, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO reconciliation_scopes (id, ledger_account_id, source, currency)
			 VALUES ($1::uuid, $2::uuid, $3, $4)`,
			scopeID, world.Accounts[0].ID, fixtureSource, fixtureCurrency); err != nil {
			return fmt.Errorf("m2 fixture: insert scope: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO match_runs
			     (id, scope_id, started_at, finished_at, status, config_hash)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'succeeded', 'm2-fixture-v1')`,
			matchRunID, scopeID, FixedInstant, FixedInstant); err != nil {
			return fmt.Errorf("m2 fixture: insert match run: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO settlement_batches
			     (id, source, period_start, period_end, declared_total_minor,
			      currency, file_hash, status, row_count, declared_row_count)
			 VALUES ($1::uuid, $2, $3::date, $3::date, 20000, $4,
			         'm2-fixture-batch-v1', 'ingested', 4, 4)`,
			batchID, fixtureSource, "2026-01-15", fixtureCurrency); err != nil {
			return fmt.Errorf("m2 fixture: insert batch: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO statement_balances
			     (id, scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
			 VALUES ($1::uuid, $2::uuid, $3::date, 0, 20000, $4, $5::uuid)`,
			statementID, scopeID, "2026-01-15", fixtureCurrency, batchID); err != nil {
			return fmt.Errorf("m2 fixture: insert statement balance: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO fee_schedules
			     (id, source, kind, currency, pct_bps, fixed_minor, valid_from)
			 VALUES ($1::uuid, $2, 'charge', $3, 0, 20, $4::date)`,
			feeScheduleID, fixtureSource, fixtureCurrency, "2026-01-01"); err != nil {
			return fmt.Errorf("m2 fixture: insert fee schedule: %w", err)
		}

		for _, txn := range world.Txns {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_txns
				     (id, created_at, booked_at, value_date, kind, idempotency_key,
				      request_hash, description, period_id)
				 VALUES ($1::uuid, $2, $3, $4::date, $5, $6, $7, $8, $9::uuid)`,
				txn.ID, txn.CreatedAt, txn.BookedAt, txn.ValueDate, txn.Kind,
				txn.IdempotencyKey, txn.RequestHash, txn.Description, FixedPeriodID); err != nil {
				return fmt.Errorf("m2 fixture: insert ledger txn %s: %w", txn.IdempotencyKey, err)
			}
		}
		for _, entry := range world.Entries {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries
				     (id, txn_id, account_id, direction, amount_minor, currency,
				      posted_at, external_ref)
				 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8)`,
				entry.ID, entry.TxnID, entry.AccountID, entry.Direction,
				entry.AmountMinor, entry.Currency, entry.PostedAt, entry.ExternalRef); err != nil {
				return fmt.Errorf("m2 fixture: insert ledger entry %s: %w", entry.ID, err)
			}
		}

		for _, record := range world.ExternalRecords {
			if _, err := tx.Exec(ctx,
				`INSERT INTO external_records
				     (id, source, source_record_id, kind, amount_minor, currency,
				      fee_minor, occurred_at, settled_at, value_date, batch_id,
				      raw, raw_hash)
				 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::date,
				         $11::uuid, $12::jsonb, $13)`,
				record.ID, fixtureSource, record.SourceRecordID, record.Kind,
				record.AmountMinor, fixtureCurrency, record.FeeMinor,
				record.OccurredAt, record.SettledAt, record.ValueDate, batchID,
				rawJSON(record.Raw), signature("m2-raw-"+record.SourceRecordID)); err != nil {
				return fmt.Errorf("m2 fixture: insert external record %s: %w", record.SourceRecordID, err)
			}
		}

		for _, brk := range world.Breaks {
			external, err := json.Marshal(brk.ExternalRefs)
			if err != nil {
				return fmt.Errorf("m2 fixture: encode break refs: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO breaks
				     (id, signature, scope_id, match_run_id, side, ledger_txn_ids,
				      external_refs, delta_minor, currency, status, detected_at)
				 VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6::text[]::uuid[],
				         $7::jsonb, $8, $9, $10, $11)`,
				brk.ID, brk.Signature, scopeID, matchRunID, brk.Side,
				brk.LedgerTxnIDs, string(external), brk.DeltaMinor, brk.Currency,
				brk.Status, brk.DetectedAt); err != nil {
				return fmt.Errorf("m2 fixture: insert break %s: %w", brk.ID, err)
			}
		}

		for _, c := range world.Cases {
			if _, err := tx.Exec(ctx,
				`INSERT INTO cases
				     (id, priority, value_minor, currency, status, version,
				      attempts, created_at)
				 VALUES ($1::uuid, $2, $3, $4, 'open', 0, 0, $5)`,
				c.ID, c.Priority, c.ValueMinor, c.Currency, c.CreatedAt); err != nil {
				return fmt.Errorf("m2 fixture: insert case %s: %w", c.ID, err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO case_breaks (case_id, break_id, created_at)
				 VALUES ($1::uuid, $2::uuid, $3)`,
				c.ID, c.BreakID, FixedInstant); err != nil {
				return fmt.Errorf("m2 fixture: link case %s: %w", c.ID, err)
			}
		}
		return nil
	})
}

type snapshotQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// CaseSnapshot is the deterministic case projection used by the two-clone
// equality test.
type CaseSnapshot struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Version    int       `json:"version"`
	Attempts   int       `json:"attempts"`
	ValueMinor int64     `json:"value_minor"`
	CreatedAt  time.Time `json:"created_at"`
}

// BreakSnapshot is the deterministic break projection.
type BreakSnapshot struct {
	ID           string    `json:"id"`
	Signature    string    `json:"signature"`
	Side         string    `json:"side"`
	DeltaMinor   int64     `json:"delta_minor"`
	LedgerTxnIDs []string  `json:"ledger_txn_ids"`
	ExternalRefs string    `json:"external_refs"`
	DetectedAt   time.Time `json:"detected_at"`
}

// Snapshot is the fixed, comparable fixture projection.
type Snapshot struct {
	Cases           []CaseSnapshot   `json:"cases"`
	Breaks          []BreakSnapshot  `json:"breaks"`
	LedgerTxns      []map[string]any `json:"ledger_txns"`
	LedgerEntries   []map[string]any `json:"ledger_entries"`
	ExternalRecords []map[string]any `json:"external_records"`
	Accounts        []map[string]any `json:"accounts"`
}

// ReadSnapshot reads the fixture projection through any query surface. It is
// used by the fixture determinism test and by the gate's provenance log.
func ReadSnapshot(ctx context.Context, q snapshotQueryer) (Snapshot, error) {
	var snap Snapshot
	rows, err := q.Query(ctx,
		`SELECT id::text, status, version, attempts, value_minor, created_at
		   FROM cases WHERE id = ANY($1::uuid[]) ORDER BY id`, caseIDs())
	if err != nil {
		return Snapshot{}, fmt.Errorf("m2 fixture: read cases: %w", err)
	}
	snap.Cases, err = collectRows(rows, func(row pgx.Rows) (CaseSnapshot, error) {
		var c CaseSnapshot
		return c, row.Scan(&c.ID, &c.Status, &c.Version, &c.Attempts, &c.ValueMinor, &c.CreatedAt)
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("m2 fixture: scan cases: %w", err)
	}

	rows, err = q.Query(ctx,
		`SELECT id::text, signature, side, delta_minor, ledger_txn_ids,
		        external_refs::text, detected_at
		   FROM breaks WHERE id = ANY($1::uuid[]) ORDER BY id`, breakIDs())
	if err != nil {
		return Snapshot{}, fmt.Errorf("m2 fixture: read breaks: %w", err)
	}
	snap.Breaks, err = collectRows(rows, func(row pgx.Rows) (BreakSnapshot, error) {
		var b BreakSnapshot
		return b, row.Scan(&b.ID, &b.Signature, &b.Side, &b.DeltaMinor,
			&b.LedgerTxnIDs, &b.ExternalRefs, &b.DetectedAt)
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("m2 fixture: scan breaks: %w", err)
	}

	snap.LedgerTxns, err = readStringMap(ctx, q,
		`SELECT id::text, kind, value_date::text, created_at::text, description
		   FROM ledger_txns WHERE id = ANY($1::uuid[]) ORDER BY id`, []any{txnIDs()},
		[]string{"id", "kind", "value_date", "created_at", "description"})
	if err != nil {
		return Snapshot{}, err
	}
	snap.LedgerEntries, err = readStringMap(ctx, q,
		`SELECT id::text, txn_id::text, account_id::text, direction,
		        amount_minor::text, currency, posted_at::text, external_ref
		   FROM ledger_entries WHERE txn_id = ANY($1::uuid[]) ORDER BY id`, []any{txnIDs()},
		[]string{"id", "txn_id", "account_id", "direction", "amount_minor",
			"currency", "posted_at", "external_ref"})
	if err != nil {
		return Snapshot{}, err
	}
	snap.ExternalRecords, err = readStringMap(ctx, q,
		`SELECT id::text, source_record_id, amount_minor::text, fee_minor::text,
		        value_date::text, raw::text
		   FROM external_records WHERE source = $1 ORDER BY id`, []any{fixtureSource},
		[]string{"id", "source_record_id", "amount_minor", "fee_minor", "value_date", "raw"})
	if err != nil {
		return Snapshot{}, err
	}
	snap.Accounts, err = readStringMap(ctx, q,
		`SELECT id::text, code, currency, status FROM accounts
		  WHERE id = ANY($1::uuid[]) ORDER BY id`, []any{accountIDs()},
		[]string{"id", "code", "currency", "status"})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func caseIDs() []string {
	ids := make([]string, 0, len(world.Cases))
	for _, c := range world.Cases {
		ids = append(ids, c.ID)
	}
	return ids
}

func breakIDs() []string {
	ids := make([]string, 0, len(world.Breaks))
	for _, b := range world.Breaks {
		ids = append(ids, b.ID)
	}
	return ids
}

func txnIDs() []string {
	ids := make([]string, 0, len(world.Txns))
	for _, t := range world.Txns {
		ids = append(ids, t.ID)
	}
	return ids
}

func accountIDs() []string {
	ids := make([]string, 0, len(world.Accounts))
	for _, a := range world.Accounts {
		ids = append(ids, a.ID)
	}
	return ids
}

func collectRows[T any](rows pgx.Rows, scan func(pgx.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	var values []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func readStringMap(ctx context.Context, q snapshotQueryer, sql string, args []any, columns []string) ([]map[string]any, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("m2 fixture: read projection: %w", err)
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		item := make(map[string]any, len(columns))
		for i, column := range columns {
			item[column] = values[i]
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
