//go:build integration

package e2e

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// TestReadModelObservableFacts covers the read-model contract: counterparty is
// the reconciliation scope source, resolution is derived from proposals.type,
// ledger amounts come from ledger_entries on the scope clearing account, and an
// external record's scope is selected by (source, currency).
func TestReadModelObservableFacts(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	caseID, usdRef := env.newCase(500)
	proposalID := env.newProposal(caseID, "adjustment", adjustmentPayload(600),
		[]string{"case:" + caseID}, nil)
	pipeline := env.pipeline(nil)

	caseFacts, err := pipeline.Loader.LoadCaseFacts(ctx, caseID)
	if err != nil {
		t.Fatalf("LoadCaseFacts: %v", err)
	}
	if caseFacts.CaseID != caseID || caseFacts.ScopeID != env.scopeID {
		t.Fatalf("case facts identity = %+v", caseFacts)
	}
	if caseFacts.Counterparty != env.source {
		t.Fatalf("case counterparty = %q, want scope source %q", caseFacts.Counterparty, env.source)
	}
	if caseFacts.BreakKind != "other" {
		t.Fatalf("case break kind = %q, want other for an external_only break", caseFacts.BreakKind)
	}
	if caseFacts.DeltaMinor != 500 || caseFacts.Currency != "USD" {
		t.Fatalf("case delta/currency = %d/%s, want 500/USD", caseFacts.DeltaMinor, caseFacts.Currency)
	}

	proposalFacts, err := pipeline.Loader.LoadProposalFacts(ctx, proposalID)
	if err != nil {
		t.Fatalf("LoadProposalFacts: %v", err)
	}
	if proposalFacts.Resolution != "adjustment" {
		t.Fatalf("resolution = %q, want adjustment derived from type", proposalFacts.Resolution)
	}
	if proposalFacts.AmountMinor != 600 || proposalFacts.Currency != "USD" {
		t.Fatalf("proposal amount/currency = %d/%s, want debit total 600/USD",
			proposalFacts.AmountMinor, proposalFacts.Currency)
	}
	if proposalFacts.Counterparty != env.source {
		t.Fatalf("proposal counterparty = %q, want scope source %q",
			proposalFacts.Counterparty, env.source)
	}
	if len(proposalFacts.LedgerRefs) != 0 || len(proposalFacts.ExternalRefs) != 0 {
		t.Fatalf("adjustment proposal cites refs: %+v", proposalFacts)
	}

	// Ledger effect: debit 700 and credit 100 on the clearing account leave a
	// net 600. A third credit in suspense keeps the direct fixture balanced but
	// must not affect the clearing projection.
	var txnID string
	if err := db.WithTx(ctx, env.tdb.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger_txns
			     (created_at, booked_at, value_date, kind, idempotency_key,
			      request_hash, description, period_id)
			 VALUES ($1, $1, $2::date, 'adjustment', 'e2e-ledger-effect',
			         'e2e-request-hash', 'e2e ledger effect', '11111111-1111-4111-8111-111111111111')
			 RETURNING id::text`, env.clock.Now(), env.asOf.Format("2006-01-02")).Scan(&txnID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (txn_id, account_id, direction, amount_minor, currency, posted_at)
			 VALUES ($1::uuid, $2::uuid, 'debit', 700, 'USD', $4),
			        ($1::uuid, $2::uuid, 'credit', 100, 'USD', $4),
			        ($1::uuid, $3::uuid, 'credit', 600, 'USD', $4)`,
			txnID, env.clearingID, env.suspenseID, env.clock.Now())
		return err
	}); err != nil {
		t.Fatalf("insert ledger txn: %v", err)
	}
	ledger, err := pipeline.Loader.Ledger(ctx, []string{txnID})
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("ledger records = %d, want 1", len(ledger))
	}
	if ledger[0].ID != txnID || ledger[0].ScopeID != env.scopeID ||
		ledger[0].AmountMinor != 600 || ledger[0].Currency != "USD" {
		t.Fatalf("ledger record = %+v, want %s/%s/600/USD", ledger[0], txnID, env.scopeID)
	}

	// External scope comes from (source, currency): add a second scope with the
	// same source but EUR and prove the USD record maps to the USD scope.
	eurClearing := env.insertAccount("psp_clearing_eur", "asset", "EUR")
	var eurScopeID string
	if err := env.tdb.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, 'EUR') RETURNING id::text`,
		eurClearing, env.source).Scan(&eurScopeID); err != nil {
		t.Fatalf("insert EUR scope: %v", err)
	}
	eurBatch := env.insertBatch(env.source, "EUR")
	eurRef := externalRef(env.source, "e2e-rec-eur")
	if _, err := env.tdb.Pool.Exec(ctx,
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, 'e2e-rec-eur', 'charge', 250, 'EUR', 0, $2, $3::date, $4::uuid,
		         '{"raw_sentinel":"must-not-be-read"}'::jsonb, 'e2e-raw-eur')`,
		env.source, env.clock.Now(), env.asOf.Format("2006-01-02"), eurBatch); err != nil {
		t.Fatalf("insert EUR external record: %v", err)
	}
	external, err := pipeline.Loader.External(ctx, []string{usdRef, eurRef})
	if err != nil {
		t.Fatalf("External: %v", err)
	}
	byID := make(map[string]string, len(external))
	for _, record := range external {
		byID[record.ID] = record.ScopeID
		if record.Counterparty != env.source {
			t.Fatalf("external counterparty = %q, want scope source %q", record.Counterparty, env.source)
		}
	}
	if byID[usdRef] != env.scopeID {
		t.Fatalf("USD external scope = %q, want %q", byID[usdRef], env.scopeID)
	}
	if byID[eurRef] != eurScopeID {
		t.Fatalf("EUR external scope = %q, want %q", byID[eurRef], eurScopeID)
	}
}
