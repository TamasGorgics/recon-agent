//go:build integration

package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

var svcOccurredAt = time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)

type svcLedgerEntry struct {
	accountID string
	direction string
	amount    int64
}

// svcWorld is one isolated database with the chart of accounts, scope, match
// run, open period, and settlement batch the integration fixtures share.
type svcWorld struct {
	t          *testing.T
	db         *testdb.DB
	clearingID string
	feesID     string
	revenueID  string
	scopeID    string
	matchRunID string
	periodID   string
	batchID    string
	seq        int
}

func newSvcWorld(t *testing.T) *svcWorld {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	w := &svcWorld{t: t, db: d}

	w.clearingID = svcInsertAccount(t, d, "clearing_psp_a_usd", "asset")
	w.feesID = svcInsertAccount(t, d, "fees_expense_usd", "expense")
	w.revenueID = svcInsertAccount(t, d, "revenue_usd", "revenue")

	if err := d.Pool.QueryRow(ctx,
		`SELECT id::text FROM periods WHERE status = 'open' ORDER BY start_date LIMIT 1`).
		Scan(&w.periodID); err != nil {
		t.Fatalf("read open period: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', 'USD')
		 RETURNING id::text`, w.clearingID).Scan(&w.scopeID); err != nil {
		t.Fatalf("create scope: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, status, config_hash)
		 VALUES ($1::uuid, now(), 'running', 'm4-05-service-it')
		 RETURNING id::text`, w.scopeID).Scan(&w.matchRunID); err != nil {
		t.Fatalf("create match run: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, status, row_count, declared_row_count)
		 VALUES ('psp_a', 0, 'USD', $1, 'ingested', 0, 0)
		 RETURNING id::text`, "m4-05-service-it-"+d.Name).
		Scan(&w.batchID); err != nil {
		t.Fatalf("create settlement batch: %v", err)
	}
	return w
}

func svcInsertAccount(t *testing.T, d *testdb.DB, code, accountType string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active')
		 RETURNING id::text`, code, accountType).Scan(&id); err != nil {
		t.Fatalf("create account %s: %v", code, err)
	}
	return id
}

func (w *svcWorld) nextKey(prefix string) string {
	w.seq++
	return fmt.Sprintf("%s-%d", prefix, w.seq)
}

// external inserts one psp_a charge dated at svcOccurredAt and returns its
// canonical evidence ref.
func (w *svcWorld) external(recordID string, amountMinor, feeMinor int64) string {
	w.t.Helper()
	return w.externalAt(recordID, amountMinor, feeMinor, "USD", svcOccurredAt, svcOccurredAt)
}

// externalAt inserts one psp_a record with an explicit currency, occurrence
// instant, and business value date.
func (w *svcWorld) externalAt(recordID string, amountMinor, feeMinor int64, currency string, occurredAt, valueDate time.Time) string {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ('psp_a', $1, 'charge', $2, $3, $4, $5, $6::date, $7::uuid,
		         '{}'::jsonb, $8)`,
		recordID, amountMinor, currency, feeMinor, occurredAt,
		valueDate.UTC().Format("2006-01-02"), w.batchID, "raw-"+recordID); err != nil {
		w.t.Fatalf("create external record %s: %v", recordID, err)
	}
	return "ext:psp_a:" + recordID
}

// ledgerCharge inserts a balanced charge whose clearing net is netMinor and
// whose fees_expense debit is feeMinor. The fee entries are omitted when the fee
// is zero.
func (w *svcWorld) ledgerCharge(netMinor, feeMinor int64) string {
	w.t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		w.t.Fatalf("begin ledger txn: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	gross := netMinor + feeMinor
	key := w.nextKey("charge")
	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ('2026-01-02', 'charge', $1, $2, 'verifier service fixture', $3::uuid)
		 RETURNING id::text`, key, "hash:"+key, w.periodID).Scan(&txnID); err != nil {
		w.t.Fatalf("create ledger txn: %v", err)
	}
	entries := []svcLedgerEntry{
		{w.clearingID, "debit", gross},
		{w.revenueID, "credit", gross},
	}
	if feeMinor > 0 {
		entries = append(entries,
			svcLedgerEntry{w.feesID, "debit", feeMinor},
			svcLedgerEntry{w.clearingID, "credit", feeMinor},
		)
	}
	for _, entry := range entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'USD')`,
			txnID, entry.accountID, entry.direction, entry.amount); err != nil {
			w.t.Fatalf("create ledger entry: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.t.Fatalf("commit ledger txn: %v", err)
	}
	return "ledger_txn:" + txnID
}

// ledgerSimple inserts a balanced clearing/revenue txn with the requested
// clearing net and no fee entry on the world's default USD scope.
func (w *svcWorld) ledgerSimple(netMinor int64) string {
	w.t.Helper()
	return w.ledgerSimpleOn("USD", netMinor, svcOccurredAt)
}

// ledgerSimpleOn inserts a balanced clearing/revenue txn in currency on the
// default psp_a clearing account for that currency.
func (w *svcWorld) ledgerSimpleOn(currency string, netMinor int64, valueDate time.Time) string {
	w.t.Helper()
	return w.ledgerSimpleVia("clearing_psp_a_"+strings.ToLower(currency), currency, netMinor, valueDate)
}

// ledgerSimpleVia inserts a balanced clearing/revenue txn whose clearing entry
// posts to clearingCode. A different account code keeps a txn outside another
// scope's affected set.
func (w *svcWorld) ledgerSimpleVia(clearingCode, currency string, netMinor int64, valueDate time.Time) string {
	w.t.Helper()
	ctx := context.Background()
	clearingID := w.ensureAccount(clearingCode, "asset", currency)
	revenueID := w.ensureAccount("revenue_"+strings.ToLower(currency), "revenue", currency)

	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		w.t.Fatalf("begin ledger txn: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	key := w.nextKey("simple")
	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ($1::date, 'charge', $2, $3, 'verifier reference fixture', $4::uuid)
		 RETURNING id::text`,
		valueDate.UTC().Format("2006-01-02"), key, "hash:"+key, w.periodID).Scan(&txnID); err != nil {
		w.t.Fatalf("create ledger txn: %v", err)
	}
	for _, entry := range []struct {
		accountID string
		direction string
	}{
		{clearingID, "debit"},
		{revenueID, "credit"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
			txnID, entry.accountID, entry.direction, netMinor, currency); err != nil {
			w.t.Fatalf("create ledger entry: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.t.Fatalf("commit ledger txn: %v", err)
	}
	return "ledger_txn:" + txnID
}

// svcRawEntry is one hand-built ledger entry for the multi-account probes.
type svcRawEntry struct {
	accountID string
	direction string
	amount    int64
	currency  string
}

// ledgerRaw inserts a ledger txn with the exact supplied entries and returns
// its canonical evidence ref. It lets a probe post scope-account and foreign
// clearing entries in the same transaction.
func (w *svcWorld) ledgerRaw(valueDate time.Time, entries []svcRawEntry) string {
	w.t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		w.t.Fatalf("begin ledger txn: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	key := w.nextKey("raw")
	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ($1::date, 'charge', $2, $3, 'verifier scope probe', $4::uuid)
		 RETURNING id::text`,
		valueDate.UTC().Format("2006-01-02"), key, "hash:"+key, w.periodID).Scan(&txnID); err != nil {
		w.t.Fatalf("create ledger txn: %v", err)
	}
	for _, entry := range entries {
		currency := entry.currency
		if currency == "" {
			currency = "USD"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency)
			 VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
			txnID, entry.accountID, entry.direction, entry.amount, currency); err != nil {
			w.t.Fatalf("create ledger entry: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.t.Fatalf("commit ledger txn: %v", err)
	}
	return "ledger_txn:" + txnID
}

// ensureAccount returns the account for code, creating it when absent.
func (w *svcWorld) ensureAccount(code, accountType, currency string) string {
	w.t.Helper()
	ctx := context.Background()
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, $3, 'active')
		 ON CONFLICT (code) DO NOTHING`,
		code, accountType, currency); err != nil {
		w.t.Fatalf("ensure account %s: %v", code, err)
	}
	var id string
	if err := w.db.Pool.QueryRow(ctx, `SELECT id::text FROM accounts WHERE code = $1`, code).Scan(&id); err != nil {
		w.t.Fatalf("read account %s: %v", code, err)
	}
	return id
}

// scopeFor returns the psp_a reconciliation scope and match run for currency,
// creating them for a non-USD currency. The USD scope and run come from
// newSvcWorld.
func (w *svcWorld) scopeFor(currency string) (string, string) {
	w.t.Helper()
	if currency == "USD" {
		return w.scopeID, w.matchRunID
	}
	ctx := context.Background()
	ledgerAccountID := w.ensureAccount("clearing_psp_a_"+strings.ToLower(currency), "asset", currency)
	var scopeID string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_a', $2)
		 ON CONFLICT (source, currency) DO NOTHING
		 RETURNING id::text`, ledgerAccountID, currency).Scan(&scopeID); err != nil {
		// ON CONFLICT DO NOTHING returns no row; read the existing scope.
		if err := w.db.Pool.QueryRow(ctx,
			`SELECT id::text FROM reconciliation_scopes WHERE source = 'psp_a' AND currency = $1`,
			currency).Scan(&scopeID); err != nil {
			w.t.Fatalf("read scope %s: %v", currency, err)
		}
	}
	var matchRunID string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, status, config_hash)
		 VALUES ($1::uuid, now(), 'running', $2)
		 RETURNING id::text`, scopeID, "m4-05-service-it-"+currency).Scan(&matchRunID); err != nil {
		w.t.Fatalf("create match run %s: %v", currency, err)
	}
	return scopeID, matchRunID
}

// insertRate inserts one fx_rates row and returns its id.
func (w *svcWorld) insertRate(base, quote, source, rate string, asOf time.Time) string {
	w.t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO fx_rates (base, quote, rate, as_of, source)
		 VALUES ($1, $2, $3::numeric, $4, $5)
		 RETURNING id::text`, base, quote, rate, asOf, source).Scan(&id); err != nil {
		w.t.Fatalf("create fx rate %s/%s: %v", base, quote, err)
	}
	return id
}

// matchLegActive records an active match leg for txnRef against a fresh
// external record, marking the ledger txn as already matched.
func (w *svcWorld) matchLegActive(txnRef string, amountMinor int64) {
	w.t.Helper()
	ctx := context.Background()
	externalRef := w.external(w.nextKey("rec-matched"), amountMinor, 0)
	source, recordID := svcSplitExternalRef(w.t, externalRef)

	var externalID string
	if err := w.db.Pool.QueryRow(ctx,
		`SELECT id::text FROM external_records WHERE source = $1 AND source_record_id = $2`,
		source, recordID).Scan(&externalID); err != nil {
		w.t.Fatalf("read matched external: %v", err)
	}
	var matchID string
	if err := w.db.Pool.QueryRow(ctx,
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status)
		 VALUES ($1::uuid, $2::uuid, 'reference-typo-it', '1.0000', $3::uuid, 'active')
		 RETURNING id::text`, w.scopeID, externalID, w.matchRunID).Scan(&matchID); err != nil {
		w.t.Fatalf("create match: %v", err)
	}
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO match_legs (match_id, ledger_txn_id, amount_minor, currency, status)
		 VALUES ($1::uuid, $2::uuid, $3, 'USD', 'active')`,
		matchID, strings.TrimPrefix(txnRef, "ledger_txn:"), amountMinor); err != nil {
		w.t.Fatalf("create match leg: %v", err)
	}
}

func (w *svcWorld) insertSchedule(pctBPS int, fixedMinor int64) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(),
		`INSERT INTO fee_schedules
		     (source, kind, currency, pct_bps, fixed_minor, valid_from, valid_to)
		 VALUES ('psp_a', 'charge', 'USD', $1, $2, '2026-01-01', NULL)`,
		pctBPS, fixedMinor); err != nil {
		w.t.Fatalf("create fee schedule: %v", err)
	}
}

// emptyCase creates an open USD case with no breaks.
func (w *svcWorld) emptyCase() string {
	w.t.Helper()
	var caseID string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO cases (priority, value_minor, currency, status, created_at)
		 VALUES (0, 100, 'USD', 'open', now())
		 RETURNING id::text`).Scan(&caseID); err != nil {
		w.t.Fatalf("create case: %v", err)
	}
	return caseID
}

type svcBreak struct {
	ledgerRef    string
	ledgerRefs   []string
	externalRef  string
	externalRefs []string
	// currency overrides the break row currency; empty uses the scope currency.
	currency string
}

// caseWithBreaks creates a case and attaches the supplied breaks on the world's
// default psp_a/USD scope.
func (w *svcWorld) caseWithBreaks(breaks []svcBreak) string {
	w.t.Helper()
	return w.caseWithBreaksOnScope(w.scopeID, w.matchRunID, "USD", breaks)
}

// caseWithBreaksOnScope creates a case and attaches the supplied breaks on an
// explicit scope. Real cases mark attached breaks in_case; the fixture uses that
// status deliberately.
func (w *svcWorld) caseWithBreaksOnScope(scopeID, matchRunID, currency string, breaks []svcBreak) string {
	w.t.Helper()
	ctx := context.Background()
	caseID := w.emptyCase()
	for _, b := range breaks {
		ledgerIDs := []string{}
		for _, ref := range b.ledgerRefs {
			ledgerIDs = append(ledgerIDs, strings.TrimPrefix(ref, "ledger_txn:"))
		}
		if b.ledgerRef != "" {
			ledgerIDs = append(ledgerIDs, strings.TrimPrefix(b.ledgerRef, "ledger_txn:"))
		}
		var externalItems []map[string]string
		externalRefs := b.externalRefs
		if b.externalRef != "" {
			externalRefs = append(externalRefs, b.externalRef)
		}
		for _, ref := range externalRefs {
			source, recordID := svcSplitExternalRef(w.t, ref)
			externalItems = append(externalItems, map[string]string{
				"source":           source,
				"source_record_id": recordID,
			})
		}
		encoded, err := json.Marshal(externalItems)
		if err != nil {
			w.t.Fatalf("marshal break external refs: %v", err)
		}
		if len(externalItems) == 0 {
			encoded = []byte("[]")
		}
		breakCurrency := b.currency
		if breakCurrency == "" {
			breakCurrency = currency
		}
		sum := sha256.Sum256([]byte(w.nextKey(caseID + "-break")))
		var breakID string
		if err := w.db.Pool.QueryRow(ctx,
			`INSERT INTO breaks
			     (signature, scope_id, match_run_id, side, ledger_txn_ids,
			      external_refs, delta_minor, currency, status, detected_at)
			 VALUES ($1, $2::uuid, $3::uuid, 'amount_mismatch', $4::text[]::uuid[],
			         $5::jsonb, 0, $6, 'in_case', now())
			 RETURNING id::text`,
			hex.EncodeToString(sum[:]), scopeID, matchRunID,
			ledgerIDs, string(encoded), breakCurrency).Scan(&breakID); err != nil {
			w.t.Fatalf("create break: %v", err)
		}
		if _, err := w.db.Pool.Exec(ctx,
			`INSERT INTO case_breaks (case_id, break_id, created_at)
			 VALUES ($1::uuid, $2::uuid, now())`, caseID, breakID); err != nil {
			w.t.Fatalf("link break to case: %v", err)
		}
	}
	return caseID
}

func (w *svcWorld) agentRun(caseID string) string {
	w.t.Helper()
	var runID string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		w.t.Fatalf("create agent run: %v", err)
	}
	return runID
}

type svcProposal struct {
	caseID   string
	typ      string
	pattern  string
	payload  map[string]any
	evidence []string
}

func (w *svcWorld) proposal(spec svcProposal) string {
	w.t.Helper()
	runID := w.agentRun(spec.caseID)
	payload, err := json.Marshal(spec.payload)
	if err != nil {
		w.t.Fatalf("marshal payload: %v", err)
	}
	evidence, err := json.Marshal(spec.evidence)
	if err != nil {
		w.t.Fatalf("marshal evidence: %v", err)
	}
	var claimed any
	if spec.pattern != "" {
		claimed = spec.pattern
	}
	var proposalID string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, confidence,
		      evidence, pattern_claimed, status, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 'verifier service integration',
		         '0.9000', $5::jsonb, $6, 'proposed', 'state-hash', $7, now())
		 RETURNING id::text`,
		spec.caseID, runID, spec.typ, payload, evidence, claimed,
		w.nextKey("proposal")).Scan(&proposalID); err != nil {
		w.t.Fatalf("insert proposal: %v", err)
	}
	return proposalID
}

func svcSplitExternalRef(t *testing.T, ref string) (string, string) {
	t.Helper()
	rest := strings.TrimPrefix(ref, "ext:")
	source, recordID, ok := strings.Cut(rest, ":")
	if !ok || source == "" || recordID == "" {
		t.Fatalf("malformed external ref %q", ref)
	}
	return source, recordID
}

func svcLines(amountMinor int64) []map[string]any {
	return svcLinesIn(amountMinor, "USD")
}

func svcLinesIn(amountMinor int64, currency string) []map[string]any {
	suffix := strings.ToLower(currency)
	return []map[string]any{
		{
			"account_code": "fees_expense_" + suffix,
			"direction":    "debit",
			"amount_minor": amountMinor,
			"currency":     currency,
		},
		{
			"account_code": "clearing_psp_a_" + suffix,
			"direction":    "credit",
			"amount_minor": amountMinor,
			"currency":     currency,
		},
	}
}

func svcEvidence(caseID string, extra ...string) []string {
	evidence := append([]string{"case:" + caseID}, extra...)
	return evidence
}

type svcStored struct {
	Verification string
	Pattern      *string
	Status       string
	Version      int
}

func svcReadStored(t *testing.T, d *testdb.DB, proposalID string) svcStored {
	t.Helper()
	var stored svcStored
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(verification::text, ''), pattern_verified, status, version
		   FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&stored.Verification, &stored.Pattern, &stored.Status, &stored.Version); err != nil {
		t.Fatalf("read stored proposal: %v", err)
	}
	return stored
}

func svcDetails(t *testing.T, stored svcStored) Details {
	t.Helper()
	var details Details
	if err := json.Unmarshal([]byte(stored.Verification), &details); err != nil {
		t.Fatalf("unmarshal stored verification %q: %v", stored.Verification, err)
	}
	return details
}

func svcRequirePersistedFailure(t *testing.T, d *testdb.DB, proposalID, reason string) {
	t.Helper()
	stored := svcReadStored(t, d, proposalID)
	if stored.Verification == "" {
		t.Fatal("verification was not persisted")
	}
	if stored.Pattern != nil {
		t.Fatalf("pattern_verified = %q, want NULL", *stored.Pattern)
	}
	details := svcDetails(t, stored)
	if details.SchemaVersion != SchemaVersion {
		t.Fatalf("stored schema_version = %d, want %d", details.SchemaVersion, SchemaVersion)
	}
	if details.Reason != reason {
		t.Fatalf("stored reason = %q, want %q (reasons %v)", details.Reason, reason, details.Reasons)
	}
	if stored.Status != "proposed" || stored.Version != 0 {
		t.Fatalf("verifier changed status/version: %s/%d", stored.Status, stored.Version)
	}
}

func svcRequireNoWrite(t *testing.T, d *testdb.DB, proposalID string) {
	t.Helper()
	stored := svcReadStored(t, d, proposalID)
	if stored.Verification != "" || stored.Pattern != nil {
		t.Fatalf("proposal was written despite no-write outcome: %+v", stored)
	}
}

func svcService(t *testing.T, db DB) *Service {
	t.Helper()
	service, err := NewService(db, DefaultOptions())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func TestServiceFeeVariancePassPersistsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-fee-pass", 119958, 20000)
	ledgerRef := w.ledgerCharge(119958, 14996)
	// gross = 119958+20000 = 139958; 10% half-even = 13996; + 1000 fixed = 14996.
	w.insertSchedule(1000, 1000)
	caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternFeeVariance,
		payload:  map[string]any{"lines": svcLines(5004)},
		evidence: svcEvidence(caseID, externalRef, ledgerRef),
	})

	service := svcService(t, w.db.Pool)
	result, err := service.Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertPass(t, result, PatternFeeVariance, PatternFeeVariance)

	stored := svcReadStored(t, w.db, proposalID)
	if stored.Pattern == nil || *stored.Pattern != PatternFeeVariance {
		t.Fatalf("pattern_verified = %v, want %s", stored.Pattern, PatternFeeVariance)
	}
	if stored.Verification == "" {
		t.Fatal("verification was not persisted")
	}
	if stored.Status != "proposed" || stored.Version != 0 {
		t.Fatalf("verifier changed status/version: %s/%d", stored.Status, stored.Version)
	}
	details := svcDetails(t, stored)
	if details.SchemaVersion != SchemaVersion || details.ExpectedMinor != 14996 || details.ActualMinor != 14996 {
		t.Fatalf("stored details = %+v, want schema 1 and expected/actual 14996", details)
	}
	if details.Reason != "" || len(details.Reasons) != 0 {
		t.Fatalf("passing verification stored reasons: %+v", details)
	}

	// Re-running on the same unchanged proposal must produce identical bytes.
	firstVerification := stored.Verification
	if _, err := service.Verify(ctx, proposalID); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	second := svcReadStored(t, w.db, proposalID)
	if second.Verification != firstVerification {
		t.Fatalf("re-run changed verification bytes:\nfirst  %s\nsecond %s", firstVerification, second.Verification)
	}
	if second.Pattern == nil || *second.Pattern != PatternFeeVariance {
		t.Fatalf("re-run pattern_verified = %v", second.Pattern)
	}
}

// TestServiceVerifyRunsAsAppRoleThroughReadViews proves the production read
// path: the verifier loads the break, external record, ledger txn/entries, and
// fee schedules through the v_agent_* views and persists verification as the
// recon_app login instead of the migration owner.
func TestServiceVerifyRunsAsAppRoleThroughReadViews(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-app-read-grant", 119958, 20000)
	ledgerRef := w.ledgerCharge(119958, 14996)
	w.insertSchedule(1000, 1000)
	caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternFeeVariance,
		payload:  map[string]any{"lines": svcLines(5004)},
		evidence: svcEvidence(caseID, externalRef, ledgerRef),
	})

	app, err := db.OpenRole(ctx, db.RoleApp, w.db.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	defer app.Close()

	result, err := svcService(t, app.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify as recon_app: %v", err)
	}
	assertPass(t, result, PatternFeeVariance, PatternFeeVariance)

	stored := svcReadStored(t, w.db, proposalID)
	if stored.Pattern == nil || *stored.Pattern != PatternFeeVariance {
		t.Fatalf("app-role pattern_verified = %v, want %s", stored.Pattern, PatternFeeVariance)
	}
	if stored.Status != "proposed" || stored.Version != 0 {
		t.Fatalf("app-role verifier changed status/version: %s/%d", stored.Status, stored.Version)
	}
}

func TestServiceFeeVarianceFailurePersistsNullPattern(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-fee-fail", 119958, 20000)
	// Booked fee 1000 differs from the scheduled 14996, but the adjustment
	// still balances the full 19000 delta, isolating fee_mismatch.
	ledgerRef := w.ledgerCharge(119958, 1000)
	w.insertSchedule(1000, 1000)
	caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternFeeVariance,
		payload:  map[string]any{"lines": svcLines(19000)},
		evidence: svcEvidence(caseID, externalRef, ledgerRef),
	})

	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertFailure(t, result, PatternFeeVariance, PatternFeeVariance, ReasonFeeMismatch)
	svcRequirePersistedFailure(t, w.db, proposalID, ReasonFeeMismatch)
}

func TestServiceRoundingPassAndFailure(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	passExternal := w.external("rec-rounding-pass", 1000, 0)
	passLedger := w.ledgerSimple(999)
	passCase := w.caseWithBreaks([]svcBreak{{ledgerRef: passLedger, externalRef: passExternal}})
	passProposal := w.proposal(svcProposal{
		caseID:   passCase,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: svcEvidence(passCase, passExternal, passLedger),
	})

	service := svcService(t, w.db.Pool)
	passResult, err := service.Verify(ctx, passProposal)
	if err != nil {
		t.Fatalf("Verify rounding pass: %v", err)
	}
	assertPass(t, passResult, PatternRounding, PatternRounding)
	stored := svcReadStored(t, w.db, passProposal)
	if stored.Pattern == nil || *stored.Pattern != PatternRounding {
		t.Fatalf("rounding pass pattern_verified = %v", stored.Pattern)
	}

	// 10.51 rounds to 11 while 10.50 half-even rounds to 10: the one-minor
	// delta is within the cap but is not a rounding-only explanation.
	failExternal := w.external("rec-rounding-fail", 1051, 0)
	failLedger := w.ledgerSimple(1050)
	failCase := w.caseWithBreaks([]svcBreak{{ledgerRef: failLedger, externalRef: failExternal}})
	failProposal := w.proposal(svcProposal{
		caseID:   failCase,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: svcEvidence(failCase, failExternal, failLedger),
	})

	failResult, err := service.Verify(ctx, failProposal)
	if err != nil {
		t.Fatalf("Verify rounding failure: %v", err)
	}
	assertFailure(t, failResult, PatternRounding, PatternRounding, ReasonNotRounding)
	svcRequirePersistedFailure(t, w.db, failProposal, ReasonNotRounding)
}

func TestServiceBreakResolutionFailuresWriteNothing(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-break", 1000, 0)
	ledgerRef := w.ledgerSimple(999)
	service := svcService(t, w.db.Pool)

	// Zero: the break also requires the ledger txn ref, which the evidence
	// does not cite.
	zeroCase := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	zeroProposal := w.proposal(svcProposal{
		caseID:   zeroCase,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: []string{externalRef, "case:" + zeroCase},
	})
	zeroResult, err := service.Verify(ctx, zeroProposal)
	if err != nil {
		t.Fatalf("Verify zero-break case: %v", err)
	}
	assertFailure(t, zeroResult, PatternRounding, PatternRounding, ReasonBreakUnresolved)
	svcRequireNoWrite(t, w.db, zeroProposal)

	// Ambiguous: two breaks are attached to the case and every ref of both is
	// cited, so the service cannot choose an authoritative source set.
	secondLedger := w.ledgerSimple(999)
	ambiguousCase := w.caseWithBreaks([]svcBreak{
		{ledgerRef: ledgerRef, externalRef: externalRef},
		{ledgerRef: secondLedger, externalRef: externalRef},
	})
	ambiguousProposal := w.proposal(svcProposal{
		caseID:   ambiguousCase,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: svcEvidence(ambiguousCase, externalRef, ledgerRef, secondLedger),
	})
	ambiguousResult, err := service.Verify(ctx, ambiguousProposal)
	if err != nil {
		t.Fatalf("Verify ambiguous-break case: %v", err)
	}
	assertFailure(t, ambiguousResult, PatternRounding, PatternRounding, ReasonBreakUnresolved)
	svcRequireNoWrite(t, w.db, ambiguousProposal)
}

func TestServiceRollbackLeavesNoChange(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-rollback", 1000, 0)
	ledgerRef := w.ledgerSimple(999)
	caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: svcEvidence(caseID, externalRef, ledgerRef),
	})

	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin caller transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	result, err := svcService(t, tx).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify in transaction: %v", err)
	}
	assertPass(t, result, PatternRounding, PatternRounding)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback caller transaction: %v", err)
	}
	svcRequireNoWrite(t, w.db, proposalID)
}

// svcStatusChangingDB flips the proposal out of proposed just before the
// verifier's persistence UPDATE, exercising the zero-row concurrency guard.
type svcStatusChangingDB struct {
	*pgxpool.Pool
	proposalID string
	changed    bool
}

func (d *svcStatusChangingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !d.changed && strings.Contains(sql, "UPDATE proposals") {
		d.changed = true
		if _, err := d.Pool.Exec(ctx,
			`UPDATE proposals SET status = 'rejected' WHERE id = $1::uuid`, d.proposalID); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return d.Pool.Exec(ctx, sql, args...)
}

func TestServiceConcurrentTransitionIsNotVerifiable(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-stale", 1000, 0)
	ledgerRef := w.ledgerSimple(999)
	caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternRounding,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: svcEvidence(caseID, externalRef, ledgerRef),
	})

	db := &svcStatusChangingDB{Pool: w.db.Pool, proposalID: proposalID}
	if _, err := svcService(t, db).Verify(ctx, proposalID); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("Verify with concurrent transition = %v, want ErrNotVerifiable", err)
	}
	svcRequireNoWrite(t, w.db, proposalID)
}

func TestServiceMalformedPayloadLinesPersistFailure(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	externalRef := w.external("rec-payload", 1000, 0)
	ledgerRef := w.ledgerSimple(999)
	service := svcService(t, w.db.Pool)

	tests := []struct {
		name    string
		payload map[string]any
	}{
		{"empty lines", map[string]any{"lines": []any{}}},
		{"empty line object", map[string]any{"lines": []any{map[string]any{}}}},
		{"missing lines", map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
			proposalID := w.proposal(svcProposal{
				caseID:   caseID,
				typ:      "adjustment",
				pattern:  PatternRounding,
				payload:  tc.payload,
				evidence: svcEvidence(caseID, externalRef, ledgerRef),
			})
			result, err := service.Verify(ctx, proposalID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			assertFailure(t, result, PatternRounding, PatternRounding, ReasonInvalidInput)
			svcRequirePersistedFailure(t, w.db, proposalID, ReasonInvalidInput)
		})
	}
}

func TestServiceClaimAndTypeFailuresPersist(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	service := svcService(t, w.db.Pool)

	noClaimCase := w.emptyCase()
	noClaim := w.proposal(svcProposal{
		caseID:   noClaimCase,
		typ:      "adjustment",
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: []string{},
	})
	result, err := service.Verify(ctx, noClaim)
	if err != nil {
		t.Fatalf("Verify no-claim proposal: %v", err)
	}
	assertFailure(t, result, "", "", ReasonNoPattern)
	svcRequirePersistedFailure(t, w.db, noClaim, ReasonNoPattern)

	otherCase := w.emptyCase()
	otherProposal := w.proposal(svcProposal{
		caseID:   otherCase,
		typ:      "adjustment",
		pattern:  PatternOther,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: []string{},
	})
	result, err = service.Verify(ctx, otherProposal)
	if err != nil {
		t.Fatalf("Verify other proposal: %v", err)
	}
	assertFailure(t, result, PatternOther, PatternOther, ReasonPatternMismatch)
	svcRequirePersistedFailure(t, w.db, otherProposal, ReasonPatternMismatch)

	wrongTypeCase := w.emptyCase()
	wrongType := w.proposal(svcProposal{
		caseID:   wrongTypeCase,
		typ:      "link",
		pattern:  PatternFeeVariance,
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: []string{},
	})
	result, err = service.Verify(ctx, wrongType)
	if err != nil {
		t.Fatalf("Verify wrong-type proposal: %v", err)
	}
	assertFailure(t, result, PatternFeeVariance, PatternFeeVariance, ReasonPatternMismatch)
	svcRequirePersistedFailure(t, w.db, wrongType, ReasonPatternMismatch)
}

// fxFixture creates the EUR external/ledger pair used by the fx service tests
// and returns the external ref, ledger ref, and case id. The break and ledger
// txn live on the psp_a/EUR scope so the scope-account net is well defined.
func (w *svcWorld) fxFixture(recordID string, amountMinor, netMinor int64, rate string) (string, string, string) {
	w.t.Helper()
	scopeID, matchRunID := w.scopeFor("EUR")
	externalRef := w.externalAt(recordID, amountMinor, 0, "EUR", svcOccurredAt, svcOccurredAt)
	ledgerRef := w.ledgerSimpleOn("EUR", netMinor, svcOccurredAt)
	w.insertRate("USD", "EUR", "psp_a", rate, svcOccurredAt.Add(-24*time.Hour))
	return externalRef, ledgerRef, w.caseWithBreaksOnScope(scopeID, matchRunID, "EUR", []svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
}

func (w *svcWorld) fxProposal(caseID, externalRef, ledgerRef string, extra ...string) string {
	w.t.Helper()
	evidence := svcEvidence(caseID, externalRef, ledgerRef)
	evidence = append(evidence, extra...)
	return w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternFXRounding,
		payload:  map[string]any{"lines": svcLinesIn(1, "EUR")},
		evidence: evidence,
	})
}

func TestServiceFXRoundingPassPersistsPattern(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef, ledgerRef, caseID := w.fxFixture("rec-fx-pass", 91, 90, "0.9")
	fxRef := "fx:USD/EUR@2026-01-02"
	proposalID := w.fxProposal(caseID, externalRef, ledgerRef, fxRef)

	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify fx pass: %v", err)
	}
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
	if got, want := result.Details.Base, "USD"; got != want {
		t.Fatalf("Base = %q, want %q", got, want)
	}
	if got, want := result.Details.Quote, "EUR"; got != want {
		t.Fatalf("Quote = %q, want %q", got, want)
	}
	if got, want := result.Details.ExpectedMinor, int64(91); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}
	if got, want := result.Details.ActualMinor, int64(90); got != want {
		t.Fatalf("ActualMinor = %d, want %d", got, want)
	}

	stored := svcReadStored(t, w.db, proposalID)
	if stored.Pattern == nil || *stored.Pattern != PatternFXRounding {
		t.Fatalf("fx pass pattern_verified = %v, want %s", stored.Pattern, PatternFXRounding)
	}
	details := svcDetails(t, stored)
	if details.AsOf == "" || details.Reason != "" || len(details.Reasons) != 0 {
		t.Fatalf("stored fx details = %+v", details)
	}
}

func TestServiceFXRoundingSingleBaseDiscoveryPass(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef, ledgerRef, caseID := w.fxFixture("rec-fx-discovery", 91, 90, "0.9")
	// No fx evidence ref: the service must derive USD from the unique base for
	// source psp_a and quote EUR.
	proposalID := w.fxProposal(caseID, externalRef, ledgerRef)

	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify fx discovery: %v", err)
	}
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
	if got, want := result.Details.Base, "USD"; got != want {
		t.Fatalf("Base = %q, want %q", got, want)
	}
}

// TestServiceFXRoundingIgnoresCitedBase proves a cited fx ref cannot select a
// pair the stored table does not contain: the single USD/EUR row is used even
// though the evidence names GBP/EUR with an unrelated date.
func TestServiceFXRoundingIgnoresCitedBase(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef, ledgerRef, caseID := w.fxFixture("rec-fx-cited-base", 91, 90, "0.9")
	proposalID := w.fxProposal(caseID, externalRef, ledgerRef, "fx:GBP/EUR@2020-01-01")

	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify fx cited base: %v", err)
	}
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
	if got, want := result.Details.Base, "USD"; got != want {
		t.Fatalf("Base = %q, want %q", got, want)
	}
	if got, want := result.Details.Quote, "EUR"; got != want {
		t.Fatalf("Quote = %q, want %q", got, want)
	}
}

func TestServiceFXRoundingFailuresPersistNullPattern(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		recordID      string
		amount        int64
		net           int64
		currency      string
		scopeCurrency string
		rate          string
		fxRef         string
		extraRates    []struct {
			base, quote, rate string
		}
		options Options
		reason  string
	}{
		{
			name: "missing rate", recordID: "rec-fx-missing", amount: 91, net: 90, currency: "EUR",
			reason: ReasonMissingRate,
		},
		{
			name: "timestamp tie", recordID: "rec-fx-tie", amount: 91, net: 90, currency: "EUR", rate: "0.9",
			extraRates: []struct{ base, quote, rate string }{{"USD", "EUR", "0.8"}},
			reason:     ReasonAmbiguousRate,
		},
		{
			// The target currency is always the quote; a target that differs
			// from the break's ledger scope currency is a currency mismatch.
			name: "target currency differs from ledger scope", recordID: "rec-fx-quote",
			amount: 91, net: 90, currency: "USD", scopeCurrency: "EUR",
			reason: ReasonCurrencyMismatch,
		},
		{
			// The cited fx ref names USD, but the stored table is ambiguous.
			// Untrusted evidence must not select one of the bases.
			name: "cited fx ref cannot override an ambiguous base table", recordID: "rec-fx-ambiguous-cited",
			amount: 91, net: 90, currency: "EUR", rate: "0.9",
			fxRef:      "fx:USD/EUR@2026-01-02",
			extraRates: []struct{ base, quote, rate string }{{"GBP", "EUR", "0.8"}},
			reason:     ReasonAmbiguousRate,
		},
		{
			name: "residual over cap", recordID: "rec-fx-cap", amount: 91, net: 90, currency: "EUR", rate: "0.9",
			options: Options{RoundingCapMinor: map[string]int64{"EUR": 0}},
			reason:  ReasonCapExceeded,
		},
		{
			name: "conversion mismatch", recordID: "rec-fx-conv", amount: 91, net: 90, currency: "EUR", rate: "2",
			reason: ReasonConversionMismatch,
		},
		{
			name: "multiple bases", recordID: "rec-fx-bases", amount: 91, net: 90, currency: "EUR", rate: "0.9",
			extraRates: []struct{ base, quote, rate string }{{"GBP", "EUR", "0.8"}},
			reason:     ReasonAmbiguousRate,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := newSvcWorld(t)
			scopeCurrency := test.scopeCurrency
			if scopeCurrency == "" {
				scopeCurrency = test.currency
			}
			scopeID, matchRunID := w.scopeFor(scopeCurrency)
			externalRef := w.externalAt(test.recordID, test.amount, 0, test.currency, svcOccurredAt, svcOccurredAt)
			ledgerRef := w.ledgerSimpleOn(scopeCurrency, test.net, svcOccurredAt)
			if test.rate != "" {
				w.insertRate("USD", test.currency, "psp_a", test.rate, svcOccurredAt.Add(-24*time.Hour))
			}
			for _, extra := range test.extraRates {
				w.insertRate(extra.base, extra.quote, "psp_a", extra.rate, svcOccurredAt.Add(-24*time.Hour))
			}
			caseID := w.caseWithBreaksOnScope(scopeID, matchRunID, scopeCurrency, []svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})

			evidence := svcEvidence(caseID, externalRef, ledgerRef)
			if test.fxRef != "" {
				evidence = append(evidence, test.fxRef)
			}
			proposalID := w.proposal(svcProposal{
				caseID:   caseID,
				typ:      "adjustment",
				pattern:  PatternFXRounding,
				payload:  map[string]any{"lines": svcLinesIn(1, test.currency)},
				evidence: evidence,
			})

			service, err := NewService(w.db.Pool, test.options)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			result, err := service.Verify(ctx, proposalID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			assertFailure(t, result, PatternFXRounding, PatternFXRounding, test.reason)
			svcRequirePersistedFailure(t, w.db, proposalID, test.reason)
		})
	}
}

func TestServiceReferenceTypoPassPersistsPattern(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef := w.external("rec-ref-pass", 1000, 0)
	candidateRef := w.ledgerSimple(1000)
	caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "link",
		pattern:  PatternReferenceTypo,
		payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": strings.TrimPrefix(candidateRef, "ledger_txn:")},
		evidence: svcEvidence(caseID, externalRef),
	})

	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify reference pass: %v", err)
	}
	assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
	if got, want := result.Details.Candidates, 1; got != want {
		t.Fatalf("Candidates = %d, want %d", got, want)
	}
	if got, want := result.Details.ExpectedMinor, int64(1000); got != want {
		t.Fatalf("ExpectedMinor = %d, want %d", got, want)
	}

	stored := svcReadStored(t, w.db, proposalID)
	if stored.Pattern == nil || *stored.Pattern != PatternReferenceTypo {
		t.Fatalf("reference pass pattern_verified = %v, want %s", stored.Pattern, PatternReferenceTypo)
	}
}

func TestServiceReferenceTypoCandidateFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("no candidate", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-none", 1000, 0)
		_ = w.ledgerSimple(999)
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "link",
			pattern:  PatternReferenceTypo,
			payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonNoCandidate)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonNoCandidate)
	})

	t.Run("two candidates", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-two", 1000, 0)
		first := w.ledgerSimple(1000)
		_ = w.ledgerSimple(1000)
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "link",
			pattern:  PatternReferenceTypo,
			payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": strings.TrimPrefix(first, "ledger_txn:")},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonMultipleCandidates)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonMultipleCandidates)
	})

	t.Run("proposed txn outside scope", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-outside", 1000, 0)
		_ = w.ledgerSimple(1000)
		outside := w.ledgerSimpleVia("clearing_psp_b_usd", "USD", 1000, svcOccurredAt)
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "link",
			pattern:  PatternReferenceTypo,
			payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": strings.TrimPrefix(outside, "ledger_txn:")},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonCandidateMismatch)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonCandidateMismatch)
	})
}

func TestServiceReferenceTypoExcludesAlreadyMatched(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef := w.external("rec-ref-matched", 1000, 0)
	matchedRef := w.ledgerSimple(1000)
	openRef := w.ledgerSimple(1000)
	w.matchLegActive(matchedRef, 1000)
	caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})

	openID := strings.TrimPrefix(openRef, "ledger_txn:")
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "link",
		pattern:  PatternReferenceTypo,
		payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": openID},
		evidence: svcEvidence(caseID, externalRef),
	})
	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify matched exclusion: %v", err)
	}
	assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
	if got, want := result.Details.Candidates, 1; got != want {
		t.Fatalf("Candidates = %d, want %d", got, want)
	}

	// The already-matched txn is not a candidate at all, so proposing it is a
	// mismatch against the sole remaining eligible txn.
	matchedID := strings.TrimPrefix(matchedRef, "ledger_txn:")
	secondCase := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
	secondProposal := w.proposal(svcProposal{
		caseID:   secondCase,
		typ:      "link",
		pattern:  PatternReferenceTypo,
		payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": matchedID},
		evidence: svcEvidence(secondCase, externalRef),
	})
	secondResult, err := svcService(t, w.db.Pool).Verify(ctx, secondProposal)
	if err != nil {
		t.Fatalf("Verify matched proposal: %v", err)
	}
	assertFailure(t, secondResult, PatternReferenceTypo, PatternReferenceTypo, ReasonCandidateMismatch)
}

func TestServiceReferenceTypoValueDateWindowEdges(t *testing.T) {
	ctx := context.Background()
	valueDate := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	// The occurrence instant is deliberately 8 days after the value date, so a
	// candidate 3 days after the value date is outside the OccurredAt window.
	occurredAt := valueDate.AddDate(0, 0, 8)

	tests := []struct {
		name        string
		candidateAt time.Time
		wantPass    bool
	}{
		{"plus three days", valueDate.AddDate(0, 0, 3), true},
		{"plus four days", valueDate.AddDate(0, 0, 4), false},
		{"minus three days", valueDate.AddDate(0, 0, -3), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := newSvcWorld(t)
			externalRef := w.externalAt("rec-ref-window", 1000, 0, "USD", occurredAt, valueDate)
			candidateRef := w.ledgerSimpleOn("USD", 1000, test.candidateAt)
			caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
			proposalID := w.proposal(svcProposal{
				caseID:   caseID,
				typ:      "link",
				pattern:  PatternReferenceTypo,
				payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": strings.TrimPrefix(candidateRef, "ledger_txn:")},
				evidence: svcEvidence(caseID, externalRef),
			})
			result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if test.wantPass {
				assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
				return
			}
			assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonNoCandidate)
			svcRequirePersistedFailure(t, w.db, proposalID, ReasonNoCandidate)
		})
	}
}

func TestServiceReferenceTypoPayloadValidation(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef := w.external("rec-ref-payload", 1000, 0)
	candidateRef := w.ledgerSimple(1000)
	candidateID := strings.TrimPrefix(candidateRef, "ledger_txn:")
	service := svcService(t, w.db.Pool)

	tests := []struct {
		name    string
		payload map[string]any
		pass    bool
	}{
		{
			name:    "both singular and plural",
			payload: map[string]any{"ledger_txn_id": candidateID, "ledger_txn_ids": []string{candidateID}},
		},
		{
			name:    "neither form",
			payload: map[string]any{},
		},
		{
			name:    "unknown field",
			payload: map[string]any{"ledger_txn_id": candidateID, "extra": "nope"},
		},
		{
			name:    "empty plural",
			payload: map[string]any{"ledger_txn_ids": []string{}},
		},
		{
			name:    "uppercase uuid",
			payload: map[string]any{"ledger_txn_id": strings.ToUpper(candidateID)},
		},
		{
			name:    "non uuid",
			payload: map[string]any{"ledger_txn_id": "ledger-ref-1"},
		},
		{
			name:    "plural duplicate normalizes",
			payload: map[string]any{"ledger_txn_ids": []string{candidateID, candidateID}},
			pass:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
			payload := make(map[string]any, len(test.payload)+1)
			payload["external_ref"] = externalRef
			for key, value := range test.payload {
				payload[key] = value
			}
			proposalID := w.proposal(svcProposal{
				caseID:   caseID,
				typ:      "link",
				pattern:  PatternReferenceTypo,
				payload:  payload,
				evidence: svcEvidence(caseID, externalRef),
			})
			result, err := service.Verify(ctx, proposalID)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if test.pass {
				assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
				return
			}
			assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonInvalidInput)
			svcRequirePersistedFailure(t, w.db, proposalID, ReasonInvalidInput)
		})
	}

	t.Run("missing external ref", func(t *testing.T) {
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "link",
			pattern:  PatternReferenceTypo,
			payload:  map[string]any{"ledger_txn_id": candidateID},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := service.Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonInvalidInput)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonInvalidInput)
	})
}

func TestServiceReferenceTypoRequiresLinkType(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)
	externalRef := w.external("rec-ref-type", 1000, 0)
	candidateRef := w.ledgerSimple(1000)
	caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		pattern:  PatternReferenceTypo,
		payload:  map[string]any{"external_ref": externalRef, "ledger_txn_id": strings.TrimPrefix(candidateRef, "ledger_txn:")},
		evidence: svcEvidence(caseID, externalRef),
	})
	result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonPatternMismatch)
	svcRequirePersistedFailure(t, w.db, proposalID, ReasonPatternMismatch)
}

// TestServiceAdjustmentSourcesBindToResolvedBreak proves adjustment loaders use
// the resolved break's exact source pair, never whatever refs happen to be in
// evidence. An external-only or ledger-only break cannot be turned into a
// passing adjustment by citing an unrelated source row.
func TestServiceAdjustmentSourcesBindToResolvedBreak(t *testing.T) {
	ctx := context.Background()

	t.Run("external-only break ignores cited ledger", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-bind-ext-only", 119958, 20000)
		ledgerRef := w.ledgerCharge(119958, 14996)
		w.insertSchedule(1000, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternFeeVariance,
			payload:  map[string]any{"lines": svcLines(5004)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternFeeVariance, PatternFeeVariance, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("ledger-only break ignores cited external", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-bind-ledger-only", 119958, 20000)
		ledgerRef := w.ledgerCharge(119958, 14996)
		w.insertSchedule(1000, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternFeeVariance,
			payload:  map[string]any{"lines": svcLines(5004)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternFeeVariance, PatternFeeVariance, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("multiple break ledger refs ambiguous", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-bind-multi-ledger", 119958, 20000)
		first := w.ledgerCharge(119958, 14996)
		second := w.ledgerCharge(119958, 14996)
		w.insertSchedule(1000, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{
			externalRef: externalRef,
			ledgerRefs:  []string{first, second},
		}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternFeeVariance,
			payload:  map[string]any{"lines": svcLines(5004)},
			evidence: svcEvidence(caseID, externalRef, first, second),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternFeeVariance, PatternFeeVariance, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("extra cited refs are ignored", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-bind-extra", 119958, 20000)
		ledgerRef := w.ledgerCharge(119958, 14996)
		extraExternal := w.external("rec-bind-extra-unrelated", 119958, 20000)
		extraLedger := w.ledgerCharge(119958, 14996)
		w.insertSchedule(1000, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "adjustment",
			pattern: PatternFeeVariance,
			payload: map[string]any{"lines": svcLines(5004)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef,
				extraExternal, extraLedger),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
	})
}

// TestServiceAdjustmentNetUsesScopeAccount proves the ledger net is the signed
// net on the break scope's ledger account only. Another clearing_* account in
// the same transaction must not inflate the amount (fee_variance/rounding/fx),
// and a transaction with no entry on the scope account is ambiguous.
func TestServiceAdjustmentNetUsesScopeAccount(t *testing.T) {
	ctx := context.Background()

	t.Run("rounding ignores other clearing entries", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-net-rounding", 1000, 0)
		otherClearing := w.ensureAccount("clearing_adv_round_usd", "asset", "USD")
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{w.clearingID, "debit", 1, "USD"},
			{otherClearing, "debit", 998, "USD"},
			{w.revenueID, "credit", 999, "USD"},
		})
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternRounding,
			payload:  map[string]any{"lines": svcLines(1)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// Scope net is 1, so the real delta is 999: over the one-minor cap.
		assertFailure(t, result, PatternRounding, PatternRounding, ReasonCapExceeded)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonCapExceeded)
	})

	t.Run("fx ignores other clearing entries", func(t *testing.T) {
		w := newSvcWorld(t)
		scopeID, matchRunID := w.scopeFor("EUR")
		externalRef := w.externalAt("rec-net-fx", 91, 0, "EUR", svcOccurredAt, svcOccurredAt)
		scopeAccount := w.ensureAccount("clearing_psp_a_eur", "asset", "EUR")
		otherClearing := w.ensureAccount("clearing_adv_fx_eur", "asset", "EUR")
		revenue := w.ensureAccount("revenue_eur", "revenue", "EUR")
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{scopeAccount, "debit", 90, "EUR"},
			{otherClearing, "debit", 1, "EUR"},
			{revenue, "credit", 91, "EUR"},
		})
		w.insertRate("USD", "EUR", "psp_a", "0.9", svcOccurredAt.Add(-24*time.Hour))
		caseID := w.caseWithBreaksOnScope(scopeID, matchRunID, "EUR",
			[]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternFXRounding,
			payload:  map[string]any{"lines": svcLinesIn(1, "EUR")},
			evidence: svcEvidence(caseID, externalRef, ledgerRef, "fx:USD/EUR@2026-01-02"),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// Scope net is 90; adding the other clearing entry (91) would make the
		// conversion inexplicable and reject this otherwise-normal fx pass.
		assertPass(t, result, PatternFXRounding, PatternFXRounding)
	})

	t.Run("absent scope entry is ambiguous", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-net-absent", 1000, 0)
		otherClearing := w.ensureAccount("clearing_adv_absent_usd", "asset", "USD")
		otherRevenue := w.ensureAccount("revenue_adv_absent_usd", "revenue", "USD")
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{otherClearing, "debit", 999, "USD"},
			{otherRevenue, "credit", 999, "USD"},
		})
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternRounding,
			payload:  map[string]any{"lines": svcLines(1)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternRounding, PatternRounding, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("fee variance ignores other clearing entries", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-net-fee", 119958, 20000)
		otherClearing := w.ensureAccount("clearing_adv_fee_usd", "asset", "USD")
		otherRevenue := w.ensureAccount("revenue_adv_fee_usd", "revenue", "USD")
		// scope net = 134954 - 14996 = 119958; booked fee = 14996.
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{w.clearingID, "debit", 134954, "USD"},
			{w.revenueID, "credit", 134954, "USD"},
			{w.feesID, "debit", 14996, "USD"},
			{w.clearingID, "credit", 14996, "USD"},
			{otherClearing, "debit", 999, "USD"},
			{otherRevenue, "credit", 999, "USD"},
		})
		w.insertSchedule(1000, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef, externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:   caseID,
			typ:      "adjustment",
			pattern:  PatternFeeVariance,
			payload:  map[string]any{"lines": svcLines(5004)},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertPass(t, result, PatternFeeVariance, PatternFeeVariance)
	})
}

// TestServiceReferenceCandidateUsesScopeNet proves the candidate amount is the
// signed net on the scope ledger account only. A transaction whose scope net is
// 1 but whose other clearing account carries 999 is not a 1000 candidate.
func TestServiceReferenceCandidateUsesScopeNet(t *testing.T) {
	ctx := context.Background()

	t.Run("other clearing does not inflate candidate", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-scope-net-fail", 1000, 0)
		otherClearing := w.ensureAccount("clearing_adv_ref_usd", "asset", "USD")
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{w.clearingID, "debit", 1, "USD"},
			{otherClearing, "debit", 999, "USD"},
			{w.revenueID, "credit", 1000, "USD"},
		})
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(ledgerRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonNoCandidate)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonNoCandidate)
	})

	t.Run("scope net candidate passes despite other clearing", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-scope-net-pass", 1000, 0)
		otherClearing := w.ensureAccount("clearing_adv_ref2_usd", "asset", "USD")
		ledgerRef := w.ledgerRaw(svcOccurredAt, []svcRawEntry{
			{w.clearingID, "debit", 1000, "USD"},
			{otherClearing, "debit", 999, "USD"},
			{w.revenueID, "credit", 1999, "USD"},
		})
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(ledgerRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
	})
}

// TestServiceReferenceTypoBindsToBreak proves a link is bound to the resolved
// break's external ref and, when present, the break's ledger set.
func TestServiceReferenceTypoBindsToBreak(t *testing.T) {
	ctx := context.Background()

	t.Run("payload external ref must equal break", func(t *testing.T) {
		w := newSvcWorld(t)
		breakExternal := w.external("rec-ref-bind-ext", 1000, 0)
		payloadExternal := w.external("rec-ref-bind-other", 1000, 0)
		candidateRef := w.ledgerSimple(1000)
		caseID := w.caseWithBreaks([]svcBreak{{externalRef: breakExternal}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  payloadExternal,
				"ledger_txn_id": strings.TrimPrefix(candidateRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, breakExternal),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("break ledger set must equal proposed set", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-bind-ledgers", 1000, 0)
		breakLedger := w.ledgerSimple(1000)
		otherLedger := w.ledgerSimple(1000)
		// The break's ledger txn is already matched, so only otherLedger is an
		// eligible scope candidate; proposing it must still fail the break-set
		// binding.
		w.matchLegActive(breakLedger, 1000)
		caseID := w.caseWithBreaks([]svcBreak{{
			externalRef: externalRef,
			ledgerRef:   breakLedger,
		}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(otherLedger, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef, breakLedger),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonCandidateMismatch)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonCandidateMismatch)
	})

	t.Run("break ledger set is the proposed set", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-bind-set-pass", 1000, 0)
		ledgerRef := w.ledgerSimple(1000)
		caseID := w.caseWithBreaks([]svcBreak{{
			externalRef: externalRef,
			ledgerRef:   ledgerRef,
		}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(ledgerRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)
	})

	t.Run("ledger-only break has no external to bind", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-bind-extraneous-ext", 1000, 0)
		ledgerRef := w.ledgerSimple(1000)
		caseID := w.caseWithBreaks([]svcBreak{{ledgerRef: ledgerRef}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(ledgerRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef, ledgerRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonSourceAmbiguous)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonSourceAmbiguous)
	})

	t.Run("break currency must match external", func(t *testing.T) {
		w := newSvcWorld(t)
		externalRef := w.external("rec-ref-bind-currency", 1000, 0)
		ledgerRef := w.ledgerSimple(1000)
		caseID := w.caseWithBreaks([]svcBreak{{
			externalRef: externalRef,
			currency:    "EUR",
		}})
		proposalID := w.proposal(svcProposal{
			caseID:  caseID,
			typ:     "link",
			pattern: PatternReferenceTypo,
			payload: map[string]any{
				"external_ref":  externalRef,
				"ledger_txn_id": strings.TrimPrefix(ledgerRef, "ledger_txn:"),
			},
			evidence: svcEvidence(caseID, externalRef),
		})
		result, err := svcService(t, w.db.Pool).Verify(ctx, proposalID)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertFailure(t, result, PatternReferenceTypo, PatternReferenceTypo, ReasonCurrencyMismatch)
		svcRequirePersistedFailure(t, w.db, proposalID, ReasonCurrencyMismatch)
	})
}

// TestServiceAppRoleReferenceAndFX proves the production read path for the
// reference and fx loaders under a real recon_app login after source binding.
func TestServiceAppRoleReferenceAndFX(t *testing.T) {
	ctx := context.Background()

	w := newSvcWorld(t)
	app, err := db.OpenRole(ctx, db.RoleApp, w.db.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	defer app.Close()

	externalRef := w.external("rec-app-ref", 1000, 0)
	candidateRef := w.ledgerSimple(1000)
	caseID := w.caseWithBreaks([]svcBreak{{externalRef: externalRef}})
	proposalID := w.proposal(svcProposal{
		caseID:  caseID,
		typ:     "link",
		pattern: PatternReferenceTypo,
		payload: map[string]any{
			"external_ref":  externalRef,
			"ledger_txn_id": strings.TrimPrefix(candidateRef, "ledger_txn:"),
		},
		evidence: svcEvidence(caseID, externalRef),
	})
	result, err := svcService(t, app.Pool).Verify(ctx, proposalID)
	if err != nil {
		t.Fatalf("app-role reference Verify: %v", err)
	}
	assertPass(t, result, PatternReferenceTypo, PatternReferenceTypo)

	w2 := newSvcWorld(t)
	app2, err := db.OpenRole(ctx, db.RoleApp, w2.db.DSNFor(testdb.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool 2: %v", err)
	}
	defer app2.Close()
	externalRef2, ledgerRef2, caseID2 := w2.fxFixture("rec-app-fx", 91, 90, "0.9")
	fxProposal := w2.fxProposal(caseID2, externalRef2, ledgerRef2, "fx:USD/EUR@2026-01-02")
	result, err = svcService(t, app2.Pool).Verify(ctx, fxProposal)
	if err != nil {
		t.Fatalf("app-role fx Verify: %v", err)
	}
	assertPass(t, result, PatternFXRounding, PatternFXRounding)
}

func TestServiceRequestValidation(t *testing.T) {
	ctx := context.Background()
	w := newSvcWorld(t)

	if _, err := NewService(nil, DefaultOptions()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService(nil) = %v, want ErrInvalidInput", err)
	}
	if _, err := NewService(w.db.Pool, Options{RoundingCapMinor: map[string]int64{"usd": 1}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewService(invalid options) = %v, want ErrInvalidOptions", err)
	}

	service := svcService(t, w.db.Pool)
	if _, err := service.Verify(ctx, "not-a-uuid"); !errors.Is(err, ErrInvalidProposalID) {
		t.Fatalf("Verify(malformed id) = %v, want ErrInvalidProposalID", err)
	}
	if _, err := service.Verify(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("Verify(missing proposal) = %v, want ErrProposalNotFound", err)
	}

	caseID := w.emptyCase()
	proposalID := w.proposal(svcProposal{
		caseID:   caseID,
		typ:      "adjustment",
		payload:  map[string]any{"lines": svcLines(1)},
		evidence: []string{},
	})
	if _, err := w.db.Pool.Exec(ctx,
		`UPDATE proposals SET status = 'rejected' WHERE id = $1::uuid`, proposalID); err != nil {
		t.Fatalf("reject proposal: %v", err)
	}
	if _, err := service.Verify(ctx, proposalID); !errors.Is(err, ErrNotVerifiable) {
		t.Fatalf("Verify(non-proposed) = %v, want ErrNotVerifiable", err)
	}
}
