//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const testNow = "2026-06-15T12:00:00Z"

var fixtureSeq atomic.Int64

// fixture is one scope with one external source and a match run, plus a case
// service running through the unprivileged recon_app role so the migration's
// grants are exercised rather than bypassed with the maintenance pool.
type fixture struct {
	db      *testdb.DB
	pool    *pgxpool.Pool // recon_app role
	service *cases.Service
	clock   *clock.Fake
	scopeID string
	batchID string
	runID   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 30})
	ctx := context.Background()

	accountID := insertAccount(t, d)
	var scopeID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD') RETURNING id::text`, accountID).Scan(&scopeID); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	batchID := insertBatch(t, d)
	runID := insertRun(t, d, scopeID)

	appPool, err := db.NewPool(ctx, d.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(appPool.Close)

	clk := clock.NewFake(mustTime(t, testNow))
	actor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	svc, err := cases.NewService(appPool, clk, actor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}
	return &fixture{db: d, pool: appPool, service: svc, clock: clk, scopeID: scopeID, batchID: batchID, runID: runID}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func insertAccount(t *testing.T, d *testdb.DB) string {
	t.Helper()
	code := fmt.Sprintf("cases_clearing_%d", fixtureSeq.Add(1))
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, 'asset', 'USD', 'active') RETURNING id::text`, code).Scan(&id); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func insertBatch(t *testing.T, d *testdb.DB) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("cases-batch-%d", n)))
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 0, 0) RETURNING id::text`,
		hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	return id
}

func insertRun(t *testing.T, d *testdb.DB, scopeID string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'fixture') RETURNING id::text`,
		scopeID, mustTime(t, testNow)).Scan(&id); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	return id
}

// addExternal inserts one external record and returns the D-5 reference.
func (f *fixture) addExternal(t *testing.T, sourceRecordID, kind string, amountMinor int64, batchID string) match.RecordRef {
	t.Helper()
	sum := sha256.Sum256([]byte(sourceRecordID))
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ('psp_a', $1, $2, $3, 'USD', 0, $4, $5, $6::uuid, '{}'::jsonb, $7)`,
		sourceRecordID, kind, amountMinor, mustTime(t, testNow),
		mustTime(t, testNow).Truncate(24*time.Hour), batchID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert external record %q: %v", sourceRecordID, err)
	}
	return match.RecordRef{Source: "psp_a", SourceRecordID: sourceRecordID}
}

// syntheticLedgerID returns a valid UUID-shaped id. Breaks carry ledger ids in
// a UUID[] column but deliberately have no per-element FK: composite breaks can
// name several claimed transactions and the signature is the identity.
func syntheticLedgerID() string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", fixtureSeq.Add(1))
}

func (f *fixture) createBreaks(t *testing.T, candidates ...match.BreakCandidate) cases.CreateBreaksResult {
	t.Helper()
	result, err := f.service.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: f.runID,
		Breaks:     candidates,
	})
	if err != nil {
		t.Fatalf("create breaks: %v", err)
	}
	return result
}

func (f *fixture) getCase(t *testing.T, caseID string) cases.Case {
	t.Helper()
	result, err := f.service.Get(context.Background(), caseID)
	if err != nil {
		t.Fatalf("get case %s: %v", caseID, err)
	}
	return result
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func (f *fixture) auditCount(t *testing.T, entityID, action string) int {
	t.Helper()
	return f.count(t,
		`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action = $2`, entityID, action)
}

func (f *fixture) auditTotal(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM audit_log`)
}

// externalOnlyCandidate builds a one-record break candidate.
func externalOnlyCandidate(t *testing.T, scopeID, currency string, ref match.RecordRef, deltaMinor int64) match.BreakCandidate {
	t.Helper()
	return match.BreakCandidate{
		ScopeID:         scopeID,
		Side:            match.SideExternalOnly,
		ExternalRecords: []match.RecordRef{ref},
		Currency:        currency,
		DeltaMinor:      deltaMinor,
	}
}

// insertBreakRow inserts a break without going through CreateBreaks, so tests
// can exercise CreateCase directly. The signature still comes from the canonical
// match helper.
func (f *fixture) insertBreakRow(t *testing.T, candidate match.BreakCandidate) string {
	t.Helper()
	refs, err := json.Marshal(candidate.ExternalRecords)
	if err != nil {
		t.Fatalf("marshal refs: %v", err)
	}
	ledgerIDs := candidate.LedgerTxnIDs
	if ledgerIDs == nil {
		ledgerIDs = []string{}
	}
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[],
		         $6::jsonb, $7, $8, 'open', $9)
		 RETURNING id::text`,
		candidate.Signature(), f.scopeID, f.runID, string(candidate.Side),
		ledgerIDs, string(refs), candidate.DeltaMinor, candidate.Currency,
		f.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert break row: %v", err)
	}
	return id
}
