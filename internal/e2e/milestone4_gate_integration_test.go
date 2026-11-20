//go:build integration

// covers: FIN-12, FIN-20, FIN-22, FIN-23, FIN-30, FIN-31, FIN-32, FIN-33, FIN-34, FIN-54

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/approver"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	proposalstate "github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testutil"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

const (
	// e2eGateAsOf is the statement date used by the milestone gate. It is the
	// fake clock's calendar day and is after the break's value date.
	e2eGateAsOf = "2026-06-15T00:00:00Z"
	// e2eGateAmountMinor is the observable discrepancy, external record amount,
	// statement closing balance, and applied clearing effect.
	e2eGateAmountMinor = 500
	// e2eGateCounterparty is the observable scope source used for the simulated
	// approver's counterparty facts.
	e2eGateCounterparty = "psp_a"
)

// gateApprovalPayload is the stored approval.request document. The real
// channel worker decodes exactly these fields before calling a Channel.
type gateApprovalPayload struct {
	ProposalID  string `json:"proposal_id"`
	CaseID      string `json:"case_id"`
	Tier        string `json:"tier"`
	Channel     string `json:"channel"`
	RequestedAt string `json:"requested_at"`
}

// TestMilestone4Gate drives the complete milestone 4 flow on one isolated
// database: break -> proposal -> approval.request outbox -> FakeChannel
// notification -> simulated approver -> approval.Service.Decide (synthetic
// human actor, source e2e) -> executor.Apply -> applied ledger entry -> audit
// chain -> independent tie-out zero.
//
// The decision comes from the eval simulated approver through the production
// Decide path. The FakeChannel only records the notification the outbox worker
// would deliver; it is not a decider, performs no network I/O, and the request
// row is asserted still undelivered/attempt-free at the end (FIN-22).
func TestMilestone4Gate(t *testing.T) {
	ctx := context.Background()
	f := newFlow(t)
	asOf := mustParseTime(t, e2eGateAsOf)

	// Seed a real statement closing balance. The tie-out must be computed from
	// data, never skipped for a missing statement row (D-2).
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, 0, $3, 'USD', $4::uuid)`,
		f.scopeID, asOf.Format("2006-01-02"), e2eGateAmountMinor, f.batchID); err != nil {
		t.Fatalf("insert statement balance: %v", err)
	}

	created, err := f.caseSvc.CreateBreaks(ctx, cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: f.matchRunID,
		Breaks: []match.BreakCandidate{{
			ScopeID:         f.scopeID,
			Side:            match.SideExternalOnly,
			ExternalRecords: []match.RecordRef{f.external},
			Currency:        "USD",
			DeltaMinor:      e2eGateAmountMinor,
		}},
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if created.CreatedBreaks != 1 || len(created.Cases) != 1 {
		t.Fatalf("CreateBreaks result = %+v, want one break and one case", created)
	}
	f.breakID = created.Breaks[0].ID
	f.caseID = created.Cases[0].ID

	// Bind the approver's observable facts to the stored case/break/scope rather
	// than restating fixture constants. "other" is the observable break-kind
	// mapping for an external-only break, matching the eval read model.
	var (
		storedValue    int64
		storedCurrency string
		storedSide     string
		scopeSource    string
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT c.value_minor, c.currency, b.side
		   FROM cases c JOIN breaks b ON b.id = $2::uuid
		  WHERE c.id = $1::uuid`, f.caseID, f.breakID).
		Scan(&storedValue, &storedCurrency, &storedSide); err != nil {
		t.Fatalf("read stored case/break facts: %v", err)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT source FROM reconciliation_scopes WHERE id = $1::uuid`,
		f.scopeID).Scan(&scopeSource); err != nil {
		t.Fatalf("read observable scope source: %v", err)
	}
	if storedValue != e2eGateAmountMinor || storedCurrency != "USD" ||
		storedSide != string(match.SideExternalOnly) || scopeSource != e2eGateCounterparty {
		t.Fatalf("stored case/break/scope facts = %d %s %s %s, want %d USD %s %s",
			storedValue, storedCurrency, storedSide, scopeSource,
			e2eGateAmountMinor, match.SideExternalOnly, e2eGateCounterparty)
	}

	// The fixture proposal posts the missing charge to the scope's clearing
	// account, so the applied entry has a real tie-out effect instead of
	// touching only non-scope accounts.
	f.insertProposalFixtureWithLines(ctx, []map[string]any{
		{"account_code": "psp_clearing_usd", "direction": "debit", "amount_minor": e2eGateAmountMinor, "currency": "USD"},
		{"account_code": "suspense", "direction": "credit", "amount_minor": e2eGateAmountMinor, "currency": "USD"},
	})

	// Derive the proposal's observable effect from its stored payload (debit
	// total and currency), exactly as the production read model does.
	var encodedProposal []byte
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT payload FROM proposals WHERE id = $1::uuid`, f.proposalID).
		Scan(&encodedProposal); err != nil {
		t.Fatalf("read stored proposal payload: %v", err)
	}
	var proposalDoc struct {
		Lines []struct {
			Direction   string `json:"direction"`
			AmountMinor int64  `json:"amount_minor"`
			Currency    string `json:"currency"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(encodedProposal, &proposalDoc); err != nil {
		t.Fatalf("decode stored proposal payload: %v", err)
	}
	var (
		proposalDebit int64
		proposalCcy   string
	)
	for _, line := range proposalDoc.Lines {
		if line.Direction != "debit" {
			continue
		}
		proposalDebit += line.AmountMinor
		proposalCcy = line.Currency
	}
	if proposalDebit != storedValue || proposalCcy != storedCurrency {
		t.Fatalf("stored proposal debit effect = %d %s, want %d %s",
			proposalDebit, proposalCcy, storedValue, storedCurrency)
	}

	if got := f.proposalStatus(ctx); got != string(proposalstate.StatusProposed) {
		t.Fatalf("proposal status after fixture = %s, want proposed", got)
	}
	preApply, err := tieout.Compute(ctx, f.app, f.scopeID, asOf)
	if err != nil {
		t.Fatalf("pre-apply tieout.Compute: %v", err)
	}
	assertGateTieOut(t, "pre-apply", preApply, asOf, gateTieOutWant{
		ledgerBalance:     0,
		ledgerOnly:        0,
		ledgerOnlyCount:   0,
		externalOnly:      e2eGateAmountMinor,
		externalOnlyCount: 1,
		unexplained:       0,
	})
	if got := f.count(ctx, `SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+f.proposalID); got != 0 {
		t.Fatalf("ledger txns before apply = %d, want 0", got)
	}

	requested, err := f.approval.Request(ctx, f.proposalID)
	if err != nil {
		t.Fatalf("approval.Request: %v", err)
	}
	if requested.Tier != policy.TierT1 || requested.Status != string(proposalstate.StatusPendingApproval) {
		t.Fatalf("Request result = %+v, want T1 pending_approval", requested)
	}
	if requested.Channel != approval.ChannelSlack || requested.AutoApproved {
		t.Fatalf("Request result = %+v, want the slack T1 channel without auto-approval", requested)
	}
	requestKey := "approval.request:" + f.proposalID
	if got := f.count(ctx, `SELECT count(*) FROM outbox WHERE idempotency_key = $1`, requestKey); got != 1 {
		t.Fatalf("approval.request outbox rows = %d, want 1", got)
	}

	var rawPayload []byte
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE idempotency_key = $1`, requestKey).Scan(&rawPayload); err != nil {
		t.Fatalf("read approval.request payload: %v", err)
	}
	var payload gateApprovalPayload
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("decode approval.request payload: %v", err)
	}
	if payload.ProposalID != f.proposalID || payload.CaseID != f.caseID ||
		payload.Tier != string(policy.TierT1) || payload.Channel != approval.ChannelSlack {
		t.Fatalf("approval.request payload = %+v, want proposal %s case %s T1 slack",
			payload, f.proposalID, f.caseID)
	}

	// FakeChannel is the notification seam: it records exactly one message and
	// cannot decide anything.
	channel := approval.NewFakeChannel()
	externalRef, err := channel.RequestApproval(ctx, approval.ApprovalRequest{
		ProposalID:     payload.ProposalID,
		CaseID:         payload.CaseID,
		Channel:        payload.Channel,
		Tier:           policy.Tier(payload.Tier),
		Payload:        rawPayload,
		IdempotencyKey: requestKey,
	})
	if err != nil {
		t.Fatalf("FakeChannel.RequestApproval: %v", err)
	}
	if want := "fake:" + payload.Channel + ":" + f.proposalID; externalRef != want {
		t.Fatalf("FakeChannel external ref = %q, want %q", externalRef, want)
	}
	notifications := channel.Requests()
	if len(notifications) != 1 {
		t.Fatalf("FakeChannel requests = %d, want 1", len(notifications))
	}
	if notifications[0].ProposalID != f.proposalID || notifications[0].CaseID != f.caseID ||
		notifications[0].Tier != policy.TierT1 || notifications[0].Channel != approval.ChannelSlack {
		t.Fatalf("FakeChannel recorded request = %+v, want the gate proposal's T1 slack request", notifications[0])
	}
	if len(channel.Cancels()) != 0 || channel.CallbackCalls() != 0 {
		t.Fatalf("FakeChannel cancels/callbacks = %d/%d, want 0/0",
			len(channel.Cancels()), channel.CallbackCalls())
	}
	if got := f.proposalStatus(ctx); got != string(proposalstate.StatusPendingApproval) {
		t.Fatalf("proposal status after notification = %s, want pending_approval", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`,
		f.proposalID); got != 0 {
		t.Fatalf("approvals rows after notification = %d, want 0", got)
	}

	// Seed the synthetic approver with the admin pool; the determination itself
	// still runs through approval.Service.Decide as a human approver.
	if err := approver.EnsureSyntheticApprover(ctx, f.db.Pool, approver.SyntheticApproverID); err != nil {
		t.Fatalf("EnsureSyntheticApprover: %v", err)
	}
	if err := approver.EnsureSyntheticApprover(ctx, f.db.Pool, approver.SyntheticApproverID); err != nil {
		t.Fatalf("idempotent EnsureSyntheticApprover: %v", err)
	}
	if got := f.count(ctx, `SELECT count(*) FROM approvers WHERE id = $1::uuid`,
		approver.SyntheticApproverID); got != 1 {
		t.Fatalf("synthetic approvers rows = %d, want 1", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM audit_log
		WHERE actor_type = 'human' AND actor_id = $1`, approver.SyntheticApproverID); got != 0 {
		t.Fatalf("human audit rows for the synthetic approver before the decision = %d, want 0 (four-eyes)", got)
	}

	// FIN-33: an agent actor can never satisfy the approval requirement. The
	// rejected attempt must leave the proposal pending and write no approval.
	if _, err := f.approval.Decide(ctx, f.proposalID, approval.DecisionApproved,
		"agent:gate-test", approver.SyntheticSource); !errors.Is(err, approval.ErrAgentActor) {
		t.Fatalf("agent actor Decide error = %v, want ErrAgentActor", err)
	}
	if got := f.proposalStatus(ctx); got != string(proposalstate.StatusPendingApproval) {
		t.Fatalf("proposal status after agent actor rejection = %s, want pending_approval", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`,
		f.proposalID); got != 0 {
		t.Fatalf("approvals rows after agent actor rejection = %d, want 0", got)
	}

	decider, err := approver.New(f.approval, nil, approver.Config{
		ApproverID: approver.SyntheticApproverID,
		Source:     approver.SyntheticSource,
	})
	if err != nil {
		t.Fatalf("new simulated approver: %v", err)
	}
	outcome, err := decider.Act(ctx, requested,
		approver.CaseFacts{
			CaseID:       f.caseID,
			ScopeID:      f.scopeID,
			BreakKind:    "other",
			DeltaMinor:   storedValue,
			Currency:     storedCurrency,
			Counterparty: scopeSource,
		},
		approver.ProposalFacts{
			ProposalID:   f.proposalID,
			Type:         "adjustment",
			Resolution:   "adjustment",
			AmountMinor:  proposalDebit,
			Currency:     proposalCcy,
			Counterparty: scopeSource,
		})
	if err != nil {
		t.Fatalf("simulated approver Act: %v", err)
	}
	if outcome.Action != approver.ActionApprove || outcome.Rule != approver.RuleExactMatch {
		t.Fatalf("simulated approver outcome = %+v, want approve/exact_match", outcome)
	}
	if outcome.Decision.ApproverID != approver.SyntheticApproverID ||
		outcome.Decision.Decision != approval.DecisionApproved ||
		outcome.Decision.Status != string(proposalstate.StatusApproved) {
		t.Fatalf("simulated decision = %+v, want approved by %s", outcome.Decision, approver.SyntheticApproverID)
	}

	var (
		approvalChannel string
		approvalBy      string
		approvalState   string
		signatureValid  bool
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT channel, approver_id::text, decision, signature_valid
		   FROM approvals WHERE proposal_id = $1::uuid`, f.proposalID).
		Scan(&approvalChannel, &approvalBy, &approvalState, &signatureValid); err != nil {
		t.Fatalf("read approvals row: %v", err)
	}
	if approvalChannel != approver.SyntheticSource || approvalBy != approver.SyntheticApproverID ||
		approvalState != string(approval.DecisionApproved) || !signatureValid {
		t.Fatalf("approvals row = %s/%s/%s/%t, want e2e/%s/approved/true",
			approvalChannel, approvalBy, approvalState, signatureValid, approver.SyntheticApproverID)
	}
	var actorType, actorID string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE action = $1 AND entity_type = 'proposal' AND entity_id = $2`,
		approval.ActionApprovalDecided, f.proposalID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read approval.decided actor: %v", err)
	}
	if actorType != "human" || actorID != approver.SyntheticApproverID {
		t.Fatalf("approval.decided actor = %s:%s, want human:%s", actorType, actorID, approver.SyntheticApproverID)
	}
	if got := f.proposalStatus(ctx); got != string(proposalstate.StatusApproved) {
		t.Fatalf("proposal status after Decide = %s, want approved", got)
	}
	if got := f.count(ctx, `SELECT count(*) FROM approvals WHERE proposal_id = $1::uuid`,
		f.proposalID); got != 1 {
		t.Fatalf("approvals rows = %d, want 1", got)
	}

	// The adjustment targets the scope clearing account. The test fixture owns
	// the executor's adjustment allowlist for this flow, mirroring the eval
	// sandbox's dataset allowlist and the M1 tie-out template.
	applySvc := f.executorWithAdjustmentAccounts(t, "psp_clearing_usd", "suspense")
	applied, err := applySvc.Apply(ctx, f.proposalID)
	if err != nil {
		t.Fatalf("executor.Apply: %v", err)
	}
	if applied.Status != executor.StatusApplied || applied.Replayed || applied.LedgerTxnID == "" {
		t.Fatalf("Apply result = %+v, want a fresh applied result with a ledger txn", applied)
	}
	if got := f.proposalStatus(ctx); got != string(proposalstate.StatusApplied) {
		t.Fatalf("proposal status after Apply = %s, want applied", got)
	}

	var (
		entryCount  int
		clearingNet int64
		valueDate   time.Time
	)
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(CASE WHEN e.direction = 'debit'
		                                     THEN e.amount_minor ELSE -e.amount_minor END), 0)::bigint
		   FROM ledger_entries e
		   JOIN accounts a ON a.id = e.account_id
		  WHERE e.txn_id = $1::uuid AND a.code = 'psp_clearing_usd'`,
		applied.LedgerTxnID).Scan(&entryCount, &clearingNet); err != nil {
		t.Fatalf("read clearing ledger effect: %v", err)
	}
	if entryCount != 1 || clearingNet != e2eGateAmountMinor {
		t.Fatalf("clearing ledger effect = %d entries / %d minor, want 1 / %d",
			entryCount, clearingNet, e2eGateAmountMinor)
	}
	if got := f.count(ctx, `SELECT count(*) FROM ledger_entries WHERE txn_id = $1::uuid`,
		applied.LedgerTxnID); got != 2 {
		t.Fatalf("ledger entries for the applied txn = %d, want 2", got)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT value_date FROM ledger_txns WHERE id = $1::uuid`,
		applied.LedgerTxnID).Scan(&valueDate); err != nil {
		t.Fatalf("read applied value date: %v", err)
	}
	if got := valueDate.UTC().Format("2006-01-02"); got != e2eValueDate {
		t.Fatalf("applied value date = %s, want the break value date %s", got, e2eValueDate)
	}

	postApply, err := tieout.Compute(ctx, f.app, f.scopeID, asOf)
	if err != nil {
		t.Fatalf("post-apply tieout.Compute: %v", err)
	}
	assertGateTieOut(t, "post-apply", postApply, asOf, gateTieOutWant{
		ledgerBalance:     e2eGateAmountMinor,
		ledgerOnly:        e2eGateAmountMinor,
		ledgerOnlyCount:   1,
		externalOnly:      e2eGateAmountMinor,
		externalOnlyCount: 1,
		unexplained:       0,
	})
	if got := postApply.LedgerBalance.AmountMinor() - preApply.LedgerBalance.AmountMinor(); got != e2eGateAmountMinor {
		t.Fatalf("ledger balance delta = %d, want %d", got, e2eGateAmountMinor)
	}
	if got := postApply.StatementClosing.AmountMinor(); got != e2eGateAmountMinor {
		t.Fatalf("statement closing = %d, want %d", got, e2eGateAmountMinor)
	}

	// Negative control: a one-minor statement perturbation must surface as a
	// one-minor unexplained difference. This proves the zero above is computed
	// from the seeded data, not an empty or skipped tie-out (FIN-54).
	if _, err := f.db.Pool.Exec(ctx,
		`UPDATE statement_balances SET closing_minor = $1
		  WHERE scope_id = $2::uuid AND as_of_date = $3::date`,
		e2eGateAmountMinor-1, f.scopeID, asOf.Format("2006-01-02")); err != nil {
		t.Fatalf("perturb statement balance: %v", err)
	}
	perturbed, err := tieout.Compute(ctx, f.app, f.scopeID, asOf)
	if err != nil {
		t.Fatalf("perturbed tieout.Compute: %v", err)
	}
	if got := perturbed.Unexplained.AmountMinor(); got != 1 {
		t.Fatalf("perturbed unexplained = %d, want 1: %+v", got, perturbed)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`UPDATE statement_balances SET closing_minor = $1
		  WHERE scope_id = $2::uuid AND as_of_date = $3::date`,
		e2eGateAmountMinor, f.scopeID, asOf.Format("2006-01-02")); err != nil {
		t.Fatalf("restore statement balance: %v", err)
	}
	restored, err := tieout.Compute(ctx, f.app, f.scopeID, asOf)
	if err != nil {
		t.Fatalf("restored tieout.Compute: %v", err)
	}
	if !restored.Unexplained.IsZero() {
		t.Fatalf("restored unexplained = %d, want 0: %+v", restored.Unexplained.AmountMinor(), restored)
	}

	want := f.expectedAuditRegistry(applied.LedgerTxnID)
	testutil.AssertAuditCompleteness(t, f.app, want)
	testutil.AssertAuditCompletenessExact(t, f.app, want, f.scopeIDs()...)
	f.assertAuditRowCounts(ctx, applied.LedgerTxnID)
	f.assertTraceToApproverIdentity(ctx, applied.LedgerTxnID,
		approver.SyntheticApproverID, approver.SyntheticApproverDisplayName)

	report, err := audit.Verify(ctx, f.app, 1)
	if err != nil {
		t.Fatalf("audit.Verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("audit.Verify findings = %+v, want an intact chain", report.Problems)
	}
	rows, _ := f.auditCounts(ctx)
	if report.HeadSeq != int64(rows) {
		t.Fatalf("audit.Verify head = %d, want %d", report.HeadSeq, rows)
	}

	// No live channel or worker ran: both outbox rows are still undelivered
	// with zero attempts (FIN-22).
	for _, key := range []string{requestKey, "proposal.applied:" + f.proposalID} {
		var (
			delivered bool
			attempts  int
		)
		if err := f.db.Pool.QueryRow(ctx,
			`SELECT delivered_at IS NOT NULL, attempts FROM outbox WHERE idempotency_key = $1`,
			key).Scan(&delivered, &attempts); err != nil {
			t.Fatalf("read outbox row %s: %v", key, err)
		}
		if delivered || attempts != 0 {
			t.Fatalf("outbox row %s = delivered %t attempts %d, want undelivered with zero attempts",
				key, delivered, attempts)
		}
	}
}

type gateTieOutWant struct {
	ledgerBalance     int64
	ledgerOnly        int64
	ledgerOnlyCount   int
	externalOnly      int64
	externalOnlyCount int
	unexplained       int64
}

func assertGateTieOut(t *testing.T, phase string, got tieout.Result, asOf time.Time, want gateTieOutWant) {
	t.Helper()
	if got.Currency != "USD" {
		t.Errorf("%s tie-out currency = %q, want USD", phase, got.Currency)
	}
	if !got.AsOf.Equal(asOf) {
		t.Errorf("%s tie-out as-of = %s, want %s", phase, got.AsOf, asOf.Format("2006-01-02"))
	}
	if got.StatementClosing.AmountMinor() != e2eGateAmountMinor {
		t.Errorf("%s statement closing = %d, want %d", phase,
			got.StatementClosing.AmountMinor(), e2eGateAmountMinor)
	}
	if got.LedgerBalance.AmountMinor() != want.ledgerBalance {
		t.Errorf("%s ledger balance = %d, want %d", phase,
			got.LedgerBalance.AmountMinor(), want.ledgerBalance)
	}
	if got.LedgerOnly.AmountMinor() != want.ledgerOnly || got.LedgerOnlyCount != want.ledgerOnlyCount {
		t.Errorf("%s ledger-only = %d (%d items), want %d (%d items)", phase,
			got.LedgerOnly.AmountMinor(), got.LedgerOnlyCount, want.ledgerOnly, want.ledgerOnlyCount)
	}
	if got.ExternalOnly.AmountMinor() != want.externalOnly || got.ExternalOnlyCount != want.externalOnlyCount {
		t.Errorf("%s external-only = %d (%d items), want %d (%d items)", phase,
			got.ExternalOnly.AmountMinor(), got.ExternalOnlyCount, want.externalOnly, want.externalOnlyCount)
	}
	if got.Unexplained.AmountMinor() != want.unexplained {
		t.Errorf("%s unexplained = %d, want %d: %+v", phase,
			got.Unexplained.AmountMinor(), want.unexplained, got)
	}
}

// executorWithAdjustmentAccounts builds the production executor over the
// cloned executor role with a fixture-specific adjustment allowlist. The
// shared flow's executor is wired with the generic defaults; the gate needs the
// scope clearing account so the applied entry affects the tie-out.
func (f *flow) executorWithAdjustmentAccounts(t *testing.T, accounts ...string) *executor.Service {
	t.Helper()
	execLedger := ledger.NewService(f.clock, config.LedgerConfig{AdjustmentAccounts: accounts})
	stateSvc, err := proposalstate.NewService(f.exec, f.clock)
	if err != nil {
		t.Fatalf("new gate executor state service: %v", err)
	}
	svc, err := executor.NewService(executor.Options{
		Pool:          f.exec,
		Ledger:        execLedger,
		State:         stateSvc,
		Clock:         f.clock,
		ExecutorRunID: "executor:m4-14-gate",
		BatchSize:     4,
		PollInterval:  time.Millisecond,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("new gate executor service: %v", err)
	}
	return svc
}

// proposalStatus reads the stored proposal status through the app pool.
func (f *flow) proposalStatus(ctx context.Context) string {
	f.t.Helper()
	var status string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT status FROM proposals WHERE id = $1::uuid`, f.proposalID).Scan(&status); err != nil {
		f.t.Fatalf("read proposal status: %v", err)
	}
	return status
}
