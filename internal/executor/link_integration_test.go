//go:build integration

// covers: FIN-20, FIN-21, FIN-23

package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
)

// setPayload rewrites a seeded proposal's stored payload. Link/duplicate
// payloads are not part of the state hash, so the proposal's canonical state
// binding stays intact.
func (w *world) setPayload(t *testing.T, proposalID string, payload map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal proposal payload: %v", err)
	}
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE proposals SET payload = $2::jsonb WHERE id = $1::uuid`,
		proposalID, string(encoded)); err != nil {
		t.Fatalf("set proposal payload: %v", err)
	}
}

// externalRef returns the canonical ext:<source>:<record id> ref for a stored
// external record.
func (w *world) externalRef(t *testing.T, recordID string) string {
	t.Helper()
	var source, sourceRecordID string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT source, source_record_id FROM external_records WHERE id = $1::uuid`,
		recordID).Scan(&source, &sourceRecordID); err != nil {
		t.Fatalf("read external record ref: %v", err)
	}
	return "ext:" + source + ":" + sourceRecordID
}

// insertLedgerTxnAmount posts a charge whose scope-account net is the signed
// amountMinor (debit positive, credit negative).
func (w *world) insertLedgerTxnAmount(t *testing.T, valueDate string, amountMinor int64) string {
	t.Helper()
	ctx := context.Background()
	tx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture ledger tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	actor, err := audit.SystemActor("fixture")
	if err != nil {
		t.Fatalf("fixture actor: %v", err)
	}
	scopeDirection, counterDirection := ledger.Debit, ledger.Credit
	amount := amountMinor
	if amount < 0 {
		scopeDirection, counterDirection = ledger.Credit, ledger.Debit
		amount = -amount
	}
	result, err := w.ledgerSvc.Post(ctx, tx, ledger.PostRequest{
		IdempotencyKey: fmt.Sprintf("executor-fixture-amount-%d", fixtureSeq.Add(1)),
		Kind:           ledger.KindCharge,
		ValueDate:      mustDate(t, valueDate),
		Description:    "executor fixture amount transaction",
		Actor:          actor,
		Lines: []ledger.Line{
			{AccountID: w.accounts["psp_clearing_usd"], Direction: scopeDirection, AmountMinor: amount, Currency: "USD"},
			{AccountID: w.accounts["fx_gain_loss"], Direction: counterDirection, AmountMinor: amount, Currency: "USD"},
		},
	})
	if err != nil {
		t.Fatalf("post fixture amount ledger txn: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture amount ledger txn: %v", err)
	}
	return result.ID
}

// seedActiveMatch creates an active match header for externalRecordID with no
// legs, simulating a matcher claim.
func (w *world) seedActiveMatch(t *testing.T, externalRecordID string) string {
	t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO matches
		     (scope_id, external_record_id, rule, confidence, match_run_id, status)
		 VALUES ($1::uuid, $2::uuid, 'fixture_conflict', 1.0000, $3::uuid, 'active')
		 RETURNING id::text`,
		w.scopeID, externalRecordID, w.matchRun).Scan(&id); err != nil {
		t.Fatalf("seed active match: %v", err)
	}
	return id
}

// seedActiveLeg claims ledgerTxnID with an active match leg under a different
// external record, simulating a matcher claim without an external conflict on
// the tested record.
func (w *world) seedActiveLeg(t *testing.T, ledgerTxnID string) {
	t.Helper()
	ctx := context.Background()
	other := w.insertExternalRecord(t, fmt.Sprintf("conflict-%d", fixtureSeq.Add(1)), "2026-02-03", 500)
	matchID := w.seedActiveMatch(t, other)
	if _, err := w.db.Pool.Exec(ctx,
		`INSERT INTO match_legs (match_id, ledger_txn_id, amount_minor, currency)
		 VALUES ($1::uuid, $2::uuid, 500, 'USD')`, matchID, ledgerTxnID); err != nil {
		t.Fatalf("seed active match leg: %v", err)
	}
}

// syntheticMatch returns the single executor/proposal run and its header for
// the expected rule. It fails the test when the row shape or count is wrong.
func (w *world) syntheticMatch(t *testing.T, rule string) (runID, matchID, externalRecordID string) {
	t.Helper()
	if got := w.count(t,
		`SELECT count(*) FROM match_runs WHERE config_hash = $1`,
		executor.ProposalMatchRunConfigHash); got != 1 {
		t.Fatalf("synthetic match runs = %d, want 1", got)
	}
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT mr.id::text, m.id::text, m.external_record_id::text
		   FROM match_runs mr
		   JOIN matches m ON m.match_run_id = mr.id
		  WHERE mr.config_hash = $1 AND m.rule = $2`,
		executor.ProposalMatchRunConfigHash, rule).
		Scan(&runID, &matchID, &externalRecordID); err != nil {
		t.Fatalf("read synthetic %s match: %v", rule, err)
	}
	return runID, matchID, externalRecordID
}

func (w *world) assertMatchLegs(t *testing.T, matchID string, want map[string]int64) {
	t.Helper()
	rows, err := w.db.Pool.Query(context.Background(),
		`SELECT ledger_txn_id::text, amount_minor, currency
		   FROM match_legs WHERE match_id = $1::uuid ORDER BY ledger_txn_id`, matchID)
	if err != nil {
		t.Fatalf("read match legs: %v", err)
	}
	defer rows.Close()
	got := make(map[string]int64)
	for rows.Next() {
		var (
			id, currency string
			amount       int64
		)
		if err := rows.Scan(&id, &amount, &currency); err != nil {
			t.Fatalf("scan match leg: %v", err)
		}
		if currency != "USD" {
			t.Errorf("match leg %s currency = %s, want USD", id, currency)
		}
		got[id] = amount
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate match legs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("match legs = %v, want %v", got, want)
	}
	for id, amount := range want {
		if got[id] != amount {
			t.Errorf("match leg %s amount = %d, want %d", id, got[id], amount)
		}
	}
}

func (w *world) assertSyntheticRun(t *testing.T, runID string) {
	t.Helper()
	var (
		status, configHash string
		stats              []byte
		started, finished  time.Time
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT status, config_hash, stats, started_at, finished_at
		   FROM match_runs WHERE id = $1::uuid`, runID).
		Scan(&status, &configHash, &stats, &started, &finished); err != nil {
		t.Fatalf("read synthetic match run: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("match run status = %s, want succeeded", status)
	}
	if configHash != executor.ProposalMatchRunConfigHash {
		t.Errorf("match run config_hash = %s, want %s", configHash, executor.ProposalMatchRunConfigHash)
	}
	if string(stats) != "{}" {
		t.Errorf("match run stats = %s, want {}", stats)
	}
	want := mustTime(t, testNow)
	if !started.Equal(want) || !finished.Equal(want) {
		t.Errorf("match run started/finished = %s/%s, want %s", started, finished, want)
	}
}

func (w *world) assertMatchHeader(t *testing.T, matchID, externalRecordID, rule string) {
	t.Helper()
	var (
		gotRule, gotStatus, gotExternal string
		confidence                      string
	)
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT rule, confidence::text, status, external_record_id::text
		   FROM matches WHERE id = $1::uuid`, matchID).
		Scan(&gotRule, &confidence, &gotStatus, &gotExternal); err != nil {
		t.Fatalf("read match header: %v", err)
	}
	if gotRule != rule {
		t.Errorf("match rule = %s, want %s", gotRule, rule)
	}
	if confidence != "1.0000" {
		t.Errorf("match confidence = %s, want 1.0000", confidence)
	}
	if gotStatus != "active" {
		t.Errorf("match status = %s, want active", gotStatus)
	}
	if gotExternal != externalRecordID {
		t.Errorf("match external record = %s, want %s", gotExternal, externalRecordID)
	}
}

func (w *world) assertNoSyntheticMatch(t *testing.T) {
	t.Helper()
	if got := w.count(t,
		`SELECT count(*) FROM match_runs WHERE config_hash = $1`,
		executor.ProposalMatchRunConfigHash); got != 0 {
		t.Errorf("synthetic match runs = %d, want 0", got)
	}
}

func (w *world) assertNoProposalLedger(t *testing.T, proposalID string) {
	t.Helper()
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+proposalID); got != 0 {
		t.Errorf("ledger transactions for proposal = %d, want 0", got)
	}
}

// TestApplyLink covers the link happy path: a synthetic run, one active
// proposal_link header, one signed leg on the scope account, no ledger
// transaction, applied_proposals with a NULL ledger id, and break/case
// resolution.
func TestApplyLink(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, withLedger: true})
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": fixture.ledgerTxnID,
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied || result.Replayed {
		t.Fatalf("result = %+v, want applied/not replayed", result)
	}
	if result.LedgerTxnID != "" {
		t.Errorf("result ledger txn id = %q, want empty", result.LedgerTxnID)
	}
	w.assertAppliedOnce(t, fixture, "")
	w.assertNoProposalLedger(t, fixture.proposalID)

	runID, matchID, externalID := w.syntheticMatch(t, executor.RuleProposalLink)
	if externalID != fixture.externalRecordID {
		t.Errorf("match external record = %s, want %s", externalID, fixture.externalRecordID)
	}
	w.assertSyntheticRun(t, runID)
	w.assertMatchHeader(t, matchID, fixture.externalRecordID, executor.RuleProposalLink)
	w.assertMatchLegs(t, matchID, map[string]int64{fixture.ledgerTxnID: 500})
}

// TestApplyLinkExternalOnly covers the M4-05 reference rule: a break with no
// ledger transactions may propose the unique scope candidate.
func TestApplyLinkExternalOnly(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, externalAmount: 1234})
	candidate := w.insertLedgerTxnAmount(t, "2026-02-04", 1234)
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":   externalRef,
		"ledger_txn_ids": []string{candidate},
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("result = %+v, want applied", result)
	}
	w.assertAppliedOnce(t, fixture, "")
	_, matchID, _ := w.syntheticMatch(t, executor.RuleProposalLink)
	w.assertMatchLegs(t, matchID, map[string]int64{candidate: 1234})
}

// TestApplyLinkExternalOnlyCandidateMismatch proves an external-only break
// only accepts the unique eligible candidate, not an arbitrary ledger
// transaction.
func TestApplyLinkExternalOnlyCandidateMismatch(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, externalAmount: 1234})
	_ = w.insertLedgerTxnAmount(t, "2026-02-04", 1234)
	other := w.insertLedgerTxnAmount(t, "2026-02-04", 500)
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": other,
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyLinkBreakSetMismatch changes the proposed ledger set away from the
// set named by the break.
func TestApplyLinkBreakSetMismatch(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, withLedger: true})
	other := w.insertLedgerTxnAmount(t, "2026-01-15", 500)
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": other,
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyLinkActiveMatchConflict rejects a link when the external record
// already has an active match header.
func TestApplyLinkActiveMatchConflict(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, withLedger: true})
	w.seedActiveMatch(t, fixture.externalRecordID)
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": fixture.ledgerTxnID,
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyLinkActiveLegConflict rejects a link when a proposed ledger
// transaction already has an active match leg.
func TestApplyLinkActiveLegConflict(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, withLedger: true})
	w.seedActiveLeg(t, fixture.ledgerTxnID)
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": fixture.ledgerTxnID,
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyLinkRollback proves a failure after the synthetic match rows still
// rolls the whole apply transaction back.
func TestApplyLinkRollback(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "link", withExternal: true, withLedger: true})
	externalRef := w.externalRef(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref":  externalRef,
		"ledger_txn_id": fixture.ledgerTxnID,
	})
	drop := w.installTrigger(t, "test_fail_link_audit", "audit_log", "INSERT",
		`IF NEW.action = 'executor.applied' THEN RAISE EXCEPTION 'injected link audit failure'; END IF;`)

	if _, err := w.service.Apply(context.Background(), fixture.proposalID); err == nil {
		t.Fatal("Apply succeeded despite the injected audit failure")
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "approved" {
		t.Errorf("proposal status = %s, want approved after rollback", got)
	}

	drop()
	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply after dropping the trigger: %v", err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("retry result = %+v, want applied", result)
	}
	_, matchID, _ := w.syntheticMatch(t, executor.RuleProposalLink)
	w.assertMatchLegs(t, matchID, map[string]int64{fixture.ledgerTxnID: 500})
}

// TestApplyDuplicate covers the duplicate happy path: a synthetic run, an
// active duplicate header with no legs, no ledger transaction,
// applied_proposals with a NULL ledger id, and break/case resolution.
func TestApplyDuplicate(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "duplicate", withExternal: true})
	externalRef := w.externalRef(t, fixture.externalRecordID)
	// duplicate_of is optional; exercise the canonical optional field with a
	// second real external record that is not part of the break.
	original := w.insertExternalRecord(t, fmt.Sprintf("dup-original-%d", fixtureSeq.Add(1)), "2026-02-03", 1234)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref": externalRef,
		"duplicate_of": w.externalRef(t, original),
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied || result.Replayed {
		t.Fatalf("result = %+v, want applied/not replayed", result)
	}
	if result.LedgerTxnID != "" {
		t.Errorf("result ledger txn id = %q, want empty", result.LedgerTxnID)
	}
	w.assertAppliedOnce(t, fixture, "")
	w.assertNoProposalLedger(t, fixture.proposalID)

	runID, matchID, externalID := w.syntheticMatch(t, executor.RuleDuplicate)
	if externalID != fixture.externalRecordID {
		t.Errorf("match external record = %s, want %s", externalID, fixture.externalRecordID)
	}
	w.assertSyntheticRun(t, runID)
	w.assertMatchHeader(t, matchID, fixture.externalRecordID, executor.RuleDuplicate)
	w.assertMatchLegs(t, matchID, map[string]int64{})
}

// TestApplyDuplicateExternalRefNotInBreak rejects a duplicate mark whose
// external reference is not one of the break's external records.
func TestApplyDuplicateExternalRefNotInBreak(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "duplicate", withExternal: true})
	other := w.insertExternalRecord(t, fmt.Sprintf("outside-%d", fixtureSeq.Add(1)), "2026-02-03", 1234)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref": w.externalRef(t, other),
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyDuplicateActiveMatchConflict rejects a duplicate mark when the
// external record already has an active match header.
func TestApplyDuplicateActiveMatchConflict(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{proposalType: "duplicate", withExternal: true})
	w.seedActiveMatch(t, fixture.externalRecordID)
	w.setPayload(t, fixture.proposalID, map[string]any{
		"external_ref": w.externalRef(t, fixture.externalRecordID),
	})

	result, err := w.service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplyFailed || result.FailureReason != executor.FailureStateChanged {
		t.Fatalf("result = %+v, want apply_failed/state_changed", result)
	}
	w.assertNoApplySideEffects(t, fixture)
	w.assertNoSyntheticMatch(t)
}

// TestApplyLinkDuplicateReplay proves repeated link/duplicate applies replay
// without a second match run/header/leg or applied row.
func TestApplyLinkDuplicateReplay(t *testing.T) {
	for _, tc := range []struct {
		name          string
		proposalType  string
		spec          caseSpec
		payload       func(*world, *caseFixture) map[string]any
		wantMatchLegs int
		rule          string
	}{
		{
			name:         "link",
			proposalType: "link",
			spec:         caseSpec{withExternal: true, withLedger: true},
			payload: func(w *world, f *caseFixture) map[string]any {
				return map[string]any{
					"external_ref":  w.externalRefForTest(f.externalRecordID),
					"ledger_txn_id": f.ledgerTxnID,
				}
			},
			wantMatchLegs: 1,
			rule:          executor.RuleProposalLink,
		},
		{
			name:         "duplicate",
			proposalType: "duplicate",
			spec:         caseSpec{withExternal: true},
			payload: func(w *world, f *caseFixture) map[string]any {
				return map[string]any{"external_ref": w.externalRefForTest(f.externalRecordID)}
			},
			wantMatchLegs: 0,
			rule:          executor.RuleDuplicate,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.spec.proposalType = tc.proposalType
			fixture := w.newCase(t, tc.spec)
			w.setPayload(t, fixture.proposalID, tc.payload(w, fixture))

			first, err := w.service.Apply(context.Background(), fixture.proposalID)
			if err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			if first.Status != executor.StatusApplied {
				t.Fatalf("first result = %+v, want applied", first)
			}
			second, err := w.service.Apply(context.Background(), fixture.proposalID)
			if err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			if second.Status != executor.StatusReplayed || !second.Replayed {
				t.Fatalf("second result = %+v, want replayed", second)
			}
			w.assertAppliedOnce(t, fixture, "")
			runID, matchID, _ := w.syntheticMatch(t, tc.rule)
			w.assertSyntheticRun(t, runID)
			if got := w.count(t,
				`SELECT count(*) FROM match_legs WHERE match_id = $1::uuid`, matchID); got != tc.wantMatchLegs {
				t.Errorf("match legs = %d, want %d", got, tc.wantMatchLegs)
			}
			if got := w.count(t,
				`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`,
				fixture.proposalID); got != 1 {
				t.Errorf("applied rows = %d, want 1", got)
			}
			if got := w.count(t,
				`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
				"proposal.applied:"+fixture.proposalID); got != 1 {
				t.Errorf("outbox rows = %d, want 1", got)
			}
		})
	}
}

// externalRefForTest is the t.Fatal-free variant used inside table-driven
// closures.
func (w *world) externalRefForTest(recordID string) string {
	var source, sourceRecordID string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT source, source_record_id FROM external_records WHERE id = $1::uuid`,
		recordID).Scan(&source, &sourceRecordID); err != nil {
		panic(err)
	}
	return "ext:" + source + ":" + sourceRecordID
}

// TestConcurrentLinkDuplicateExactlyOneWinner races two applies of one
// link/duplicate proposal and expects exactly one synthetic match graph.
func TestConcurrentLinkDuplicateExactlyOneWinner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		proposalType string
		spec         caseSpec
		payload      func(*world, *caseFixture) map[string]any
		rule         string
		wantLegs     int
	}{
		{
			name:         "link",
			proposalType: "link",
			spec:         caseSpec{withExternal: true, withLedger: true},
			payload: func(w *world, f *caseFixture) map[string]any {
				return map[string]any{
					"external_ref":  w.externalRefForTest(f.externalRecordID),
					"ledger_txn_id": f.ledgerTxnID,
				}
			},
			rule:     executor.RuleProposalLink,
			wantLegs: 1,
		},
		{
			name:         "duplicate",
			proposalType: "duplicate",
			spec:         caseSpec{withExternal: true},
			payload: func(w *world, f *caseFixture) map[string]any {
				return map[string]any{"external_ref": w.externalRefForTest(f.externalRecordID)}
			},
			rule:     executor.RuleDuplicate,
			wantLegs: 0,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.spec.proposalType = tc.proposalType
			fixture := w.newCase(t, tc.spec)
			w.setPayload(t, fixture.proposalID, tc.payload(w, fixture))

			start := make(chan struct{})
			results := make([]executor.Result, 2)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results[i], errs[i] = w.service.Apply(context.Background(), fixture.proposalID)
				}()
			}
			close(start)
			wg.Wait()

			for i := range errs {
				if errs[i] != nil {
					t.Fatalf("Apply %d error = %v, want nil", i, errs[i])
				}
			}
			applied, replayed := 0, 0
			for _, result := range results {
				switch {
				case result.Status == executor.StatusApplied && !result.Replayed:
					applied++
				case result.Status == executor.StatusReplayed && result.Replayed:
					replayed++
				default:
					t.Fatalf("unexpected concurrent result %+v", result)
				}
			}
			if applied != 1 || replayed != 1 {
				t.Fatalf("applied/replayed = %d/%d, want 1/1", applied, replayed)
			}
			w.assertAppliedOnce(t, fixture, "")
			runID, matchID, _ := w.syntheticMatch(t, tc.rule)
			w.assertSyntheticRun(t, runID)
			if got := w.count(t,
				`SELECT count(*) FROM match_legs WHERE match_id = $1::uuid`, matchID); got != tc.wantLegs {
				t.Errorf("match legs = %d, want %d", got, tc.wantLegs)
			}
			if got := w.count(t,
				`SELECT count(*) FROM match_runs WHERE config_hash = $1`,
				executor.ProposalMatchRunConfigHash); got != 1 {
				t.Errorf("synthetic match runs = %d, want 1", got)
			}
		})
	}
}
