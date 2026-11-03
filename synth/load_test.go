//go:build integration

package synth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/synth"
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "synth: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "synth: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func loadFixture(t *testing.T) (*testdb.DB, string, *synth.Dataset) {
	t.Helper()
	dataset := generate(t, testOptions(21, 4))
	dir := t.TempDir()
	if err := dataset.Write(dir); err != nil {
		t.Fatalf("write dataset: %v", err)
	}
	return testdb.New(t), dir, dataset
}

func loadDataset(t *testing.T, pool *pgxpool.Pool, dir string) synth.LoadResult {
	t.Helper()
	actor, err := audit.SystemActor("synth")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	result, err := synth.Load(context.Background(), synth.LoadOptions{
		Dir:     dir,
		DB:      pool,
		Clock:   clock.NewFake(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)),
		Actor:   actor,
		Sources: config.IngestConfig{Sources: config.DefaultSynthIngestSources()},
		Ledger:  config.DefaultLedgerConfig(),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return result
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return count
}

// TestLoadCleanDataset pins the M1-09 acceptance contract: zero quarantine,
// one ground-truth link per external record, agreeing ledger/external/link
// counts, and one distinct clearing account per (source, currency) scope.
//
// covers: FIN-06
func TestLoadCleanDataset(t *testing.T) {
	d, dir, dataset := loadFixture(t)
	manifestData, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest synth.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	result := loadDataset(t, d.Pool, dir)
	if result.Quarantined != 0 {
		t.Fatalf("quarantined = %d, reasons = %v", result.Quarantined, result.QuarantineReasons)
	}
	if result.LedgerPosted != manifest.Counts.LedgerTxns {
		t.Errorf("ledger posted = %d, want %d", result.LedgerPosted, manifest.Counts.LedgerTxns)
	}
	if result.LedgerReplayed != 0 {
		t.Errorf("first load replayed %d transactions", result.LedgerReplayed)
	}
	if result.Files == 0 || result.NoOpFiles != 0 {
		t.Errorf("files = %d, no-op = %d, want a non-empty first ingest", result.Files, result.NoOpFiles)
	}
	if result.RecordsInserted != manifest.Counts.ExternalRecords {
		t.Errorf("records inserted = %d, want %d", result.RecordsInserted, manifest.Counts.ExternalRecords)
	}

	ledgerTxns := countRows(t, d.Pool, `SELECT count(*) FROM ledger_txns`)
	externalRecords := countRows(t, d.Pool, `SELECT count(*) FROM external_records`)
	if ledgerTxns != int64(manifest.Counts.LedgerTxns) {
		t.Errorf("ledger_txns = %d, want %d", ledgerTxns, manifest.Counts.LedgerTxns)
	}
	if externalRecords != int64(manifest.Counts.ExternalRecords) {
		t.Errorf("external_records = %d, want %d", externalRecords, manifest.Counts.ExternalRecords)
	}
	if ledgerTxns != externalRecords {
		t.Errorf("ledger_txns = %d, external_records = %d, want equal", ledgerTxns, externalRecords)
	}

	if got := countRows(t, d.Pool, `SELECT count(*) FROM settlement_batches WHERE status = 'quarantined'`); got != 0 {
		t.Errorf("quarantined batches = %d, want 0", got)
	}
	if got := countRows(t, d.Pool, `SELECT count(*) FROM settlement_batches`); got != int64(result.Files) {
		t.Errorf("settlement_batches = %d, want %d", got, result.Files)
	}

	if got := countRows(t, d.Pool, `SELECT count(*) FROM fee_schedules`); got != int64(manifest.Counts.FeeSchedules) {
		t.Errorf("fee_schedules = %d, want %d", got, manifest.Counts.FeeSchedules)
	}
	if got := countRows(t, d.Pool, `SELECT count(*) FROM accounts`); got != int64(manifest.Counts.Accounts) {
		t.Errorf("accounts = %d, want %d", got, manifest.Counts.Accounts)
	}

	// One ground-truth link per external record and no unlinked record.
	links := 0
	for _, scenario := range dataset.Truth.Scenarios {
		if len(scenario.ExternalRecords) != 1 {
			t.Errorf("scenario %s has %d links, want 1", scenario.ScenarioID, len(scenario.ExternalRecords))
		}
		for _, ref := range scenario.ExternalRecords {
			links++
			if got := countRows(t, d.Pool,
				`SELECT count(*) FROM external_records WHERE source = $1 AND source_record_id = $2`,
				ref.Source, ref.SourceRecordID); got != 1 {
				t.Errorf("external record %s/%s has %d rows, want exactly 1",
					ref.Source, ref.SourceRecordID, got)
			}
		}
	}
	if links != int(externalRecords) {
		t.Errorf("ground truth links = %d, external_records = %d, want equal", links, externalRecords)
	}

	// One distinct clearing account per (source, currency) scope.
	currencies := len(config.DefaultSynthConfig().Currencies)
	wantScopes := int64(2 * currencies)
	if got := countRows(t, d.Pool, `SELECT count(*) FROM reconciliation_scopes`); got != wantScopes {
		t.Errorf("reconciliation_scopes = %d, want %d", got, wantScopes)
	}
	if got := countRows(t, d.Pool, `SELECT count(DISTINCT ledger_account_id) FROM reconciliation_scopes`); got != wantScopes {
		t.Errorf("distinct scope accounts = %d, want %d", got, wantScopes)
	}
	rows, err := d.Pool.Query(context.Background(),
		`SELECT source, currency, count(DISTINCT ledger_account_id)
		   FROM reconciliation_scopes GROUP BY source, currency`)
	if err != nil {
		t.Fatalf("scope grouping: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source, currency string
		var accounts int64
		if err := rows.Scan(&source, &currency, &accounts); err != nil {
			t.Fatalf("scan scope grouping: %v", err)
		}
		if accounts != 1 {
			t.Errorf("scope %s/%s has %d distinct clearing accounts, want 1", source, currency, accounts)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate scope grouping: %v", err)
	}
}

// TestLoadIsIdempotent re-runs load on the same directory: ledger replays and
// ingestion file-hash no-ops, so no table grows.
func TestLoadIsIdempotent(t *testing.T) {
	d, dir, dataset := loadFixture(t)
	first := loadDataset(t, d.Pool, dir)

	tables := []string{
		"accounts", "fee_schedules", "reconciliation_scopes",
		"ledger_txns", "ledger_entries", "settlement_batches",
		"external_records", "statement_balances", "audit_log",
	}
	before := make(map[string]int64, len(tables))
	for _, table := range tables {
		before[table] = countRows(t, d.Pool, `SELECT count(*) FROM `+table)
	}

	second := loadDataset(t, d.Pool, dir)
	if second.LedgerPosted != 0 {
		t.Errorf("second load posted %d new ledger transactions, want 0", second.LedgerPosted)
	}
	if second.LedgerReplayed != first.LedgerPosted {
		t.Errorf("second load replayed %d transactions, want %d", second.LedgerReplayed, first.LedgerPosted)
	}
	if second.Files != first.Files || second.NoOpFiles != first.Files {
		t.Errorf("second load files = %d, no-op = %d, want %d no-ops",
			second.Files, second.NoOpFiles, first.Files)
	}
	if second.RecordsInserted != 0 {
		t.Errorf("second load inserted %d external records, want 0", second.RecordsInserted)
	}
	if second.Quarantined != 0 {
		t.Errorf("second load quarantined %d batches", second.Quarantined)
	}

	for _, table := range tables {
		if got := countRows(t, d.Pool, `SELECT count(*) FROM `+table); got != before[table] {
			t.Errorf("%s count = %d after re-load, want %d", table, got, before[table])
		}
	}
	if second.Accounts != len(dataset.Ledger.Accounts) ||
		second.FeeSchedules != len(dataset.Ledger.FeeSchedules) ||
		second.Scopes != len(dataset.Ledger.ReconciliationScopes) {
		t.Errorf("reference data result changed: %+v", second)
	}
}
