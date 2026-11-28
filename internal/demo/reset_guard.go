package demo

// M5-05 destructive reset guard. demo.Reset drops the public schema and the
// eval-owned schemas, so the guard must inspect every table in those schemas,
// not only the eval/proposal/approval tables. A table is allowed to contain
// rows only when each row is provably demo-owned:
//
//   - migration-seeded reference rows in currencies, periods, system_switches;
//   - rows recorded in demo_metadata by the M1/ops/quality seed layers;
//   - named operational-evidence tables produced by running the product
//     against the demo database. Today the only such table is
//     public.integrity_runs: an integrity CLI/scheduler run has no demo
//     metadata, so any rows there are allowed. demo.Reset drops that run
//     history; the exception is deliberately narrow (no wildcard, no
//     demoRowKeySQL entry) and does not cover outbox alert rows, which still
//     block reset (AQ-3b).
//
// Reset takes its own advisory lock, separate from the integrity run's
// transaction-scoped lock, so a demo environment must serialize a reset and a
// running worker scheduler by operator convention rather than by lock.
//
// Everything else aborts reset with *UntaggedDataError (errors.Is
// ErrQualityUntaggedData) before any DDL runs. The guard executes inside the
// same transaction that takes ACCESS EXCLUSIVE locks on all dropped tables and
// performs the drop, so a concurrent insert either commits before the lock (and
// is seen by the guard) or blocks and cannot be silently destroyed.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/money"
)

const (
	demoPublicSchema = "public"
	demoEvalSchema   = "eval_truth"

	// periodsSeedID is the single open period created by migration
	// 20261104090000_m1_03_ledger.sql.
	periodsSeedID = "11111111-1111-4111-8111-111111111111"
)

// UntaggedDataError reports one dropped table that contains rows which cannot
// be proven demo-owned. Errors.Is(err, ErrQualityUntaggedData) is true for
// compatibility with callers of the original eval-only guard.
type UntaggedDataError struct {
	Schema string
	Table  string
	Rows   int64
}

func (e *UntaggedDataError) Error() string {
	return fmt.Sprintf(
		"demo quality: %s.%s contains %d untagged non-demo row(s); refusing to reset a shared database",
		e.Schema, e.Table, e.Rows)
}

// Is makes the typed error match the package sentinel.
func (e *UntaggedDataError) Is(target error) bool { return target == ErrQualityUntaggedData }

// resetQueryer is the read surface shared by *sql.DB (preflight) and *sql.Tx
// (the locked guard inside the reset transaction).
type resetQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type resetTable struct {
	Schema string
	Name   string
}

// listResetTables returns every table in a schema the reset destroys. The
// names come from the PostgreSQL catalog, not user input, and are quoted with
// pgx.Identifier where they are interpolated.
func listResetTables(ctx context.Context, q resetQueryer) ([]resetTable, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT schemaname, tablename
		   FROM pg_tables
		  WHERE schemaname IN ($1, $2)
		  ORDER BY schemaname, tablename`,
		demoPublicSchema, demoEvalSchema)
	if err != nil {
		return nil, fmt.Errorf("demo quality: list reset tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []resetTable
	for rows.Next() {
		var table resetTable
		if err := rows.Scan(&table.Schema, &table.Name); err != nil {
			return nil, fmt.Errorf("demo quality: scan reset table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demo quality: iterate reset tables: %w", err)
	}
	return tables, nil
}

// lockResetTables takes one ACCESS EXCLUSIVE lock covering every table the
// reset will drop. The single statement avoids lock-order deadlocks between
// concurrent resets and blocks new writes before the guard query runs.
func lockResetTables(ctx context.Context, tx *sql.Tx) error {
	tables, err := listResetTables(ctx, tx)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil
	}
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		names = append(names, pgx.Identifier{table.Schema, table.Name}.Sanitize())
	}
	if _, err := tx.ExecContext(ctx,
		"LOCK TABLE "+strings.Join(names, ", ")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("demo quality: lock reset tables: %w", err)
	}
	return nil
}

// checkResetAllowed implements the all-tables guard. It only reads; callers
// must hold the reset table locks when the result authorises a destructive
// action.
func checkResetAllowed(ctx context.Context, q resetQueryer) error {
	tables, err := listResetTables(ctx, q)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		// A fresh, unmigrated scratch database has nothing to destroy.
		return nil
	}

	hasMetadata, err := relationExists(ctx, q, demoPublicSchema, demoMetadataTable)
	if err != nil {
		return err
	}

	for _, table := range tables {
		if table.Schema == demoPublicSchema {
			switch table.Name {
			case demoMetadataTable, "goose_db_version":
				// Infrastructure: marker rows are demo-owned by definition and
				// goose rows are recreated by the migration replay.
				continue
			case "currencies":
				if err := checkCurrencies(ctx, q); err != nil {
					return err
				}
				continue
			case "periods":
				if err := checkPeriods(ctx, q); err != nil {
					return err
				}
				continue
			case "system_switches":
				if err := checkSystemSwitches(ctx, q); err != nil {
					return err
				}
				continue
			case "integrity_runs":
				// Operational evidence from running recon integrity against
				// the demo database. demo.Reset drops this history. This is
				// the only operational-evidence exception: an outbox
				// integrity.failed row still blocks reset (AQ-3b), and no
				// other table is weakened.
				continue
			}
		}

		// eval_truth tables and any public table the demo never writes must be
		// empty; a row there is untagged evidence.
		keySQL, demoTable := demoRowKeySQL[table.Name]
		if table.Schema != demoPublicSchema || !demoTable || !hasMetadata {
			count, err := countRows(ctx, q, table)
			if err != nil {
				return err
			}
			if count > 0 {
				return &UntaggedDataError{Schema: table.Schema, Table: table.Name, Rows: count}
			}
			continue
		}

		count, err := countUntaggedRows(ctx, q, table, keySQL)
		if err != nil {
			return err
		}
		if count > 0 {
			return &UntaggedDataError{Schema: table.Schema, Table: table.Name, Rows: count}
		}
	}
	return nil
}

func relationExists(ctx context.Context, q resetQueryer, schema, name string) (bool, error) {
	var exists bool
	if err := q.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`,
		pgx.Identifier{schema, name}.Sanitize()).Scan(&exists); err != nil {
		return false, fmt.Errorf("demo quality: check relation %s.%s: %w", schema, name, err)
	}
	return exists, nil
}

func countRows(ctx context.Context, q resetQueryer, table resetTable) (int64, error) {
	var count int64
	query := "SELECT count(*) FROM " + pgx.Identifier{table.Schema, table.Name}.Sanitize()
	if err := q.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("demo quality: count %s.%s: %w", table.Schema, table.Name, err)
	}
	return count, nil
}

func countUntaggedRows(ctx context.Context, q resetQueryer, table resetTable, keySQL string) (int64, error) {
	query := fmt.Sprintf(
		`SELECT count(*)
		   FROM %s AS demo_row
		  WHERE NOT EXISTS (
		        SELECT 1
		          FROM %s AS marker
		         WHERE marker.table_name = $1
		           AND marker.row_key = %s
		  )`,
		pgx.Identifier{table.Schema, table.Name}.Sanitize(),
		pgx.Identifier{demoPublicSchema, demoMetadataTable}.Sanitize(),
		keySQL)
	var count int64
	if err := q.QueryRowContext(ctx, query, table.Name).Scan(&count); err != nil {
		return 0, fmt.Errorf("demo quality: count untagged %s.%s rows: %w",
			table.Schema, table.Name, err)
	}
	return count, nil
}

// checkCurrencies allows only the ISO 4217 rows reproduced by the M1-03
// migration. The Go money table is the single source of truth for that seed.
func checkCurrencies(ctx context.Context, q resetQueryer) error {
	allowed := make(map[string]int)
	for _, currency := range money.Currencies() {
		allowed[currency.Code()] = currency.Exponent()
	}

	rows, err := q.QueryContext(ctx, `SELECT code, exponent FROM public.currencies`)
	if err != nil {
		return fmt.Errorf("demo quality: read currencies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var untagged []string
	for rows.Next() {
		var code string
		var exponent int8
		if err := rows.Scan(&code, &exponent); err != nil {
			return fmt.Errorf("demo quality: scan currency: %w", err)
		}
		want, ok := allowed[code]
		if !ok || want != int(exponent) {
			untagged = append(untagged, code)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("demo quality: iterate currencies: %w", err)
	}
	if len(untagged) > 0 {
		sort.Strings(untagged)
		return &UntaggedDataError{
			Schema: demoPublicSchema, Table: "currencies", Rows: int64(len(untagged)),
		}
	}
	return nil
}

// checkPeriods allows only the single open period seeded by M1-03. Tests and
// real deployments create additional periods, so any other row is untagged.
func checkPeriods(ctx context.Context, q resetQueryer) error {
	var count int64
	if err := q.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM public.periods
		  WHERE id <> $1::uuid
		     OR start_date <> DATE '2020-01-01'
		     OR end_date <> DATE '2030-12-31'
		     OR status <> 'open'`,
		periodsSeedID).Scan(&count); err != nil {
		return fmt.Errorf("demo quality: check periods: %w", err)
	}
	if count > 0 {
		return &UntaggedDataError{Schema: demoPublicSchema, Table: "periods", Rows: count}
	}
	return nil
}

// checkSystemSwitches allows only the two switches seeded by M2-01. Their
// enabled value may legitimately change at runtime; the identity is the name.
func checkSystemSwitches(ctx context.Context, q resetQueryer) error {
	var count int64
	if err := q.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM public.system_switches
		  WHERE name NOT IN ('agent_enabled', 'auto_apply_enabled')`).Scan(&count); err != nil {
		return fmt.Errorf("demo quality: check system_switches: %w", err)
	}
	if count > 0 {
		return &UntaggedDataError{Schema: demoPublicSchema, Table: "system_switches", Rows: count}
	}
	return nil
}
