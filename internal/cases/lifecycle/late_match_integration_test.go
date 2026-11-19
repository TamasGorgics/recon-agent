//go:build integration

// covers: FIN-24, FIN-30, FIN-41

package lifecycle_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

const (
	lateMatchFrom = "2026-11-01"
	lateMatchTo   = "2026-11-30"
	lateValueDate = "2026-11-10"
)

// lateMatcherFixture extends the lifecycle fixture with one reconciliation
// scope and a real match.Engine whose late-match hook is the production
// lifecycle service.
type lateMatcherFixture struct {
	*fixture
	source       string
	scopeID      string
	accountID    string
	counterID    string
	batchID      string
	matcher      *match.Engine
	caseService  *cases.Service
	matcherActor audit.Actor
}

func newLateMatcherFixture(t *testing.T) *lateMatcherFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	source := "late_psp"

	accountID := insertLateAccount(t, f, source+"_clearing", "asset")
	counterID := insertLateAccount(t, f, source+"_counter", "revenue")
	batchID := insertLateBatch(t, f, source)
	var scopeID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, 'USD') RETURNING id::text`,
		accountID, source).Scan(&scopeID); err != nil {
		t.Fatalf("insert reconciliation scope: %v", err)
	}

	matcherActor, err := audit.ParseActor("system:matcher")
	if err != nil {
		t.Fatalf("parse matcher actor: %v", err)
	}
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		t.Fatalf("parse cases actor: %v", err)
	}
	caseService, err := cases.NewService(f.app, f.clock, caseActor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}
	engine, err := match.New(match.Options{
		DB:        f.app,
		Clock:     f.clock,
		Actor:     matcherActor,
		Config:    config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": 100}},
		LateMatch: f.service.ReconcileLateMatchHook,
	})
	if err != nil {
		t.Fatalf("new matcher: %v", err)
	}
	return &lateMatcherFixture{
		fixture:      f,
		source:       source,
		scopeID:      scopeID,
		accountID:    accountID,
		counterID:    counterID,
		batchID:      batchID,
		matcher:      engine,
		caseService:  caseService,
		matcherActor: matcherActor,
	}
}

func insertLateAccount(t *testing.T, f *fixture, code, typ string) string {
	t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, $2, 'USD', 'active') RETURNING id::text`,
		code, typ).Scan(&id); err != nil {
		t.Fatalf("insert account %q: %v", code, err)
	}
	return id
}

func insertLateBatch(t *testing.T, f *fixture, source string) string {
	t.Helper()
	n := fixtureSeq.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-batch-%d", source, n)))
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, declared_total_minor, currency, file_hash, row_count, declared_row_count)
		 VALUES ($1, 0, 'USD', $2, 0, 0) RETURNING id::text`,
		source, hex.EncodeToString(sum[:])).Scan(&id); err != nil {
		t.Fatalf("insert settlement batch: %v", err)
	}
	return id
}

func lateDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		t.Fatalf("parse date %q: %v", value, err)
	}
	return parsed
}

// addExternal inserts one external record in the fixture scope. It returns the
// row id and the source_record_id used in break external_refs.
func (f *lateMatcherFixture) addExternal(t *testing.T, ref string, amountMinor int64,
	valueDate string) (externalID, sourceRecordID string) {
	t.Helper()
	n := fixtureSeq.Add(1)
	sourceRecordID = fmt.Sprintf("%s-%d", ref, n)
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	raw, err := json.Marshal(map[string]string{"reference": ref})
	if err != nil {
		t.Fatalf("marshal raw payload: %v", err)
	}
	sum := sha256.Sum256(raw)
	date := lateDate(t, valueDate)
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, $3, $4, 'USD', 0, $5, $6, $7::uuid, $8::jsonb, $9)
		 RETURNING id::text`,
		f.source, sourceRecordID, kind, amountMinor, date.Add(12*time.Hour),
		date, f.batchID, string(raw), hex.EncodeToString(sum[:])).Scan(&externalID); err != nil {
		t.Fatalf("insert external record %q: %v", ref, err)
	}
	return externalID, sourceRecordID
}

// addLedger inserts one balanced ledger transaction in the fixture scope.
func (f *lateMatcherFixture) addLedger(t *testing.T, ref string, amountMinor int64,
	valueDate string) string {
	t.Helper()
	ctx := context.Background()
	n := fixtureSeq.Add(1)
	kind := "charge"
	if amountMinor < 0 {
		kind = "payout"
	}
	date := lateDate(t, valueDate)
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ledger fixture: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_txns
		     (value_date, kind, idempotency_key, request_hash, description, period_id)
		 VALUES ($1, $2, $3, 'fixture', $4, '11111111-1111-4111-8111-111111111111')
		 RETURNING id::text`,
		date, kind, fmt.Sprintf("late-%s-%d", ref, n), ref).Scan(&txnID); err != nil {
		t.Fatalf("insert ledger txn %q: %v", ref, err)
	}
	scopeDirection, counterDirection := "debit", "credit"
	if amountMinor < 0 {
		scopeDirection, counterDirection = "credit", "debit"
	}
	absolute := amountMinor
	if absolute < 0 {
		absolute = -absolute
	}
	for _, entry := range []struct {
		accountID string
		direction string
		ref       any
	}{
		{f.accountID, scopeDirection, ref},
		{f.counterID, counterDirection, nil},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries
			     (txn_id, account_id, direction, amount_minor, currency, external_ref)
			 VALUES ($1::uuid, $2::uuid, $3, $4, 'USD', $5)`,
			txnID, entry.accountID, entry.direction, absolute, entry.ref); err != nil {
			t.Fatalf("insert ledger entry for %q: %v", ref, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ledger fixture %q: %v", ref, err)
	}
	return txnID
}

func (f *lateMatcherFixture) window(t *testing.T) match.Window {
	t.Helper()
	return match.Window{From: lateDate(t, lateMatchFrom), To: lateDate(t, lateMatchTo)}
}

func (f *lateMatcherFixture) run(t *testing.T) match.RunResult {
	t.Helper()
	result, err := f.matcher.Run(context.Background(), match.RunRequest{
		ScopeID: f.scopeID,
		Window:  f.window(t),
	})
	if err != nil {
		t.Fatalf("run matcher: %v", err)
	}
	return result
}

func (f *lateMatcherFixture) runErr(t *testing.T) error {
	t.Helper()
	_, err := f.matcher.Run(context.Background(), match.RunRequest{
		ScopeID: f.scopeID,
		Window:  f.window(t),
	})
	return err
}

// createCase persists the first run's external-only break through the real
// cases service and returns the created case and break ids.
func (f *lateMatcherFixture) createCase(t *testing.T, result match.RunResult) (caseID, breakID string) {
	t.Helper()
	externalOnly := make([]match.BreakCandidate, 0, 1)
	for _, candidate := range result.Breaks {
		if candidate.Side == match.SideExternalOnly {
			externalOnly = append(externalOnly, candidate)
		}
	}
	if len(externalOnly) != 1 {
		t.Fatalf("external-only break candidates = %d, want exactly 1 (all breaks: %+v)",
			len(externalOnly), result.Breaks)
	}
	created, err := f.caseService.CreateBreaks(context.Background(), cases.CreateBreaksRequest{
		ScopeID:    f.scopeID,
		MatchRunID: result.RunID,
		Breaks:     externalOnly,
	})
	if err != nil {
		t.Fatalf("CreateBreaks: %v", err)
	}
	if len(created.Breaks) != 1 || len(created.Cases) != 1 {
		t.Fatalf("CreateBreaks created %d breaks / %d cases, want 1/1",
			len(created.Breaks), len(created.Cases))
	}
	return created.Cases[0].ID, created.Breaks[0].ID
}

// transitionCase drives the real case state authority in its own transaction.
func (f *lateMatcherFixture) transitionCase(t *testing.T, caseID string,
	to casestate.Status, actor, reason, resolution string) {
	t.Helper()
	ctx := context.Background()
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var version int
		if err := tx.QueryRow(ctx,
			`SELECT version FROM cases WHERE id = $1::uuid FOR UPDATE`, caseID).
			Scan(&version); err != nil {
			return err
		}
		_, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
			CaseID:          caseID,
			To:              to,
			ExpectedVersion: version,
			Actor:           actor,
			Reason:          reason,
			ResolutionType:  resolution,
			Now:             f.clock.Now(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("transition case %s -> %s: %v", caseID, to, err)
	}
}

// setupLateCase creates the full-consumption starting point: an external-only
// payout break, a case in proposal_pending, and one active proposed proposal.
func setupLateCase(t *testing.T) (f *lateMatcherFixture, externalID, caseID, breakID, proposalID string) {
	t.Helper()
	f = newLateMatcherFixture(t)
	externalID, _ = f.addExternal(t, "LATE-CASE", -10_000, lateValueDate)
	first := f.run(t)
	caseID, breakID = f.createCase(t, first)
	f.transitionCase(t, caseID, casestate.StatusInAgent, "system:cases", "", "")
	f.transitionCase(t, caseID, casestate.StatusProposalPending, "system:cases", "", "")
	proposalID = f.seedProposal(t, caseID, "proposed")
	return f, externalID, caseID, breakID, proposalID
}

func (f *lateMatcherFixture) matchIDForExternal(t *testing.T, externalID string) string {
	t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM matches
		  WHERE external_record_id = $1::uuid AND status = 'active'`,
		externalID).Scan(&id); err != nil {
		t.Fatalf("load active match for external %s: %v", externalID, err)
	}
	return id
}

func (f *lateMatcherFixture) breakStatus(t *testing.T, breakID string) string {
	t.Helper()
	var status string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status FROM breaks WHERE id = $1::uuid`, breakID).Scan(&status); err != nil {
		t.Fatalf("read break %s: %v", breakID, err)
	}
	return status
}

func (f *lateMatcherFixture) reconcile(ctx context.Context, matchID, actor string) (caselifecycle.LateMatchResult, error) {
	var result caselifecycle.LateMatchResult
	err := db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = f.service.ReconcileLateMatch(ctx, tx, matchID, actor)
		return err
	})
	return result, err
}

// TestLateMatchFullConsumptionRealMatcher is the M4-10 section 19 acceptance
// path. The match is created by the real match.Engine.Run; no match row is
// inserted by SQL.
func TestLateMatchFullConsumptionRealMatcher(t *testing.T) {
	f, externalID, caseID, breakID, proposalID := setupLateCase(t)
	beforeCaseAudits := len(f.audits(t, caseID))

	ledgerID := f.addLedger(t, "LATE-CASE", -10_000, lateValueDate)
	second := f.run(t)
	if second.Stats.Matched != 1 {
		t.Fatalf("second run matched = %d, want 1 (stats: %+v)", second.Stats.Matched, second.Stats)
	}
	matchID := f.matchIDForExternal(t, externalID)

	if got := f.breakStatus(t, breakID); got != "superseded" {
		t.Fatalf("break status = %s, want superseded", got)
	}
	if got := f.proposal(t, proposalID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal = %s/version %d, want superseded/1", got.Status, got.Version)
	}

	outbox := f.outbox(t)
	if len(outbox) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(outbox))
	}
	cancel := outbox[0]
	if cancel.AggregateType != caselifecycle.AggregateApproval || cancel.AggregateID != proposalID {
		t.Fatalf("outbox aggregate = %s/%s, want %s/%s", cancel.AggregateType,
			cancel.AggregateID, caselifecycle.AggregateApproval, proposalID)
	}
	if cancel.Topic != caselifecycle.TopicApprovalCancel {
		t.Fatalf("outbox topic = %s, want %s", cancel.Topic, caselifecycle.TopicApprovalCancel)
	}
	if want := caselifecycle.CancelIdempotencyKey(proposalID); cancel.IdempotencyKey != want {
		t.Fatalf("idempotency key = %q, want %q", cancel.IdempotencyKey, want)
	}
	payload := jsonObject(t, cancel.Payload)
	assertKeys(t, payload, "proposal_id", "case_id", "reason")
	if payload["proposal_id"] != proposalID || payload["case_id"] != caseID ||
		payload["reason"] != caselifecycle.ReasonLateMatch {
		t.Fatalf("cancel payload = %v, want proposal/case/superseded_by_match", payload)
	}

	storedCase := f.caseRow(t, caseID)
	if storedCase.Status != "resolved" {
		t.Fatalf("case status = %s, want resolved", storedCase.Status)
	}
	if storedCase.ResolutionType == nil ||
		*storedCase.ResolutionType != casestate.ResolutionSupersededByMatch {
		t.Fatalf("case resolution_type = %v, want %s", storedCase.ResolutionType,
			casestate.ResolutionSupersededByMatch)
	}
	if storedCase.ResolvedAt == nil {
		t.Fatal("resolved case has no resolved_at")
	}

	var (
		activeLegs int
		legTxn     string
	)
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(min(ledger_txn_id::text), '')
		   FROM match_legs WHERE match_id = $1::uuid AND status = 'active'`,
		matchID).Scan(&activeLegs, &legTxn); err != nil {
		t.Fatalf("read match legs: %v", err)
	}
	if activeLegs != 1 || legTxn != ledgerID {
		t.Fatalf("active legs = %d/%s, want 1/%s", activeLegs, legTxn, ledgerID)
	}

	caseAudits := f.audits(t, caseID)[beforeCaseAudits:]
	var transitions, supersedeAudits int
	for _, row := range caseAudits {
		switch row.Action {
		case casestate.ActionCaseTransition:
			transitions++
			if row.ActorType != "system" || row.ActorID != "matcher" {
				t.Fatalf("late transition actor = %s:%s, want system:matcher",
					row.ActorType, row.ActorID)
			}
			after := jsonObject(t, row.After)
			if after["resolution_type"] != casestate.ResolutionSupersededByMatch ||
				after["status"] != "resolved" {
				t.Fatalf("late transition after = %v, want resolved/superseded_by_match", after)
			}
		case caselifecycle.ActionCaseProposalSuperseded:
			supersedeAudits++
		}
	}
	if transitions != 1 {
		t.Fatalf("late-match case.transition audits = %d, want exactly 1", transitions)
	}
	if supersedeAudits != 1 {
		t.Fatalf("case.proposal_superseded audits = %d, want exactly 1", supersedeAudits)
	}

	var breakSuperseded int
	for _, row := range f.audits(t, breakID) {
		if row.Action != caselifecycle.ActionBreakSuperseded {
			continue
		}
		breakSuperseded++
		if row.ActorType != "system" || row.ActorID != "matcher" {
			t.Fatalf("break audit actor = %s:%s, want system:matcher", row.ActorType, row.ActorID)
		}
		after := jsonObject(t, row.After)
		if after["status"] != "superseded" || after["match_id"] != matchID ||
			after["case_id"] != caseID {
			t.Fatalf("break audit after = %v, want superseded with match/case", after)
		}
	}
	if breakSuperseded != 1 {
		t.Fatalf("break.superseded audits = %d, want exactly 1", breakSuperseded)
	}

	proposalAudits := f.audits(t, proposalID)
	if len(proposalAudits) != 1 || proposalAudits[0].Action != "proposal.transition" {
		t.Fatalf("proposal audits = %+v, want exactly one proposal.transition", proposalAudits)
	}
}

// TestLateMatchReconciliationIsIdempotent reruns reconciliation for the same
// active match and asserts every state row and audit count is unchanged.
func TestLateMatchReconciliationIsIdempotent(t *testing.T) {
	f, externalID, caseID, breakID, proposalID := setupLateCase(t)
	f.addLedger(t, "LATE-CASE", -10_000, lateValueDate)
	f.run(t)
	matchID := f.matchIDForExternal(t, externalID)

	beforeProposal := f.proposal(t, proposalID)
	beforeCase := f.caseRow(t, caseID)
	beforeBreak := f.breakStatus(t, breakID)
	beforeAudits := f.count(t, `SELECT count(*) FROM audit_log`)
	beforeOutbox := len(f.outbox(t))

	result, err := f.reconcile(context.Background(), matchID, "system:matcher")
	if err != nil {
		t.Fatalf("second ReconcileLateMatch: %v", err)
	}
	if !result.Reconciled || len(result.Cases) != 1 {
		t.Fatalf("idempotent result = %+v, want reconciled with one case", result)
	}
	if result.Cases[0].SkippedReason != caselifecycle.SkipAlreadySuperseded {
		t.Fatalf("skip reason = %q, want %q", result.Cases[0].SkippedReason,
			caselifecycle.SkipAlreadySuperseded)
	}

	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != beforeAudits {
		t.Fatalf("audit rows after replay = %d, want %d", got, beforeAudits)
	}
	if got := len(f.outbox(t)); got != beforeOutbox {
		t.Fatalf("outbox rows after replay = %d, want %d", got, beforeOutbox)
	}
	if got := f.proposal(t, proposalID); got != beforeProposal {
		t.Fatalf("proposal after replay = %+v, want %+v", got, beforeProposal)
	}
	if got := f.caseRow(t, caseID); got.Status != beforeCase.Status ||
		got.Version != beforeCase.Version {
		t.Fatalf("case after replay = %s/version %d, want %s/version %d",
			got.Status, got.Version, beforeCase.Status, beforeCase.Version)
	}
	if got := f.breakStatus(t, breakID); got != beforeBreak {
		t.Fatalf("break after replay = %s, want %s", got, beforeBreak)
	}
}

// TestLateMatchResolvedOtherResolutionIsSkipped pins the admin-resolution
// boundary: the late match audits the skip once and never rewrites the case or
// its still-open break.
func TestLateMatchResolvedOtherResolutionIsSkipped(t *testing.T) {
	f := newLateMatcherFixture(t)
	externalID, _ := f.addExternal(t, "LATE-SKIP", -10_000, lateValueDate)
	first := f.run(t)
	caseID, breakID := f.createCase(t, first)
	f.transitionCase(t, caseID, casestate.StatusResolved, "human:alice",
		"manual resolution", "manual")
	beforeCase := f.caseRow(t, caseID)

	f.addLedger(t, "LATE-SKIP", -10_000, lateValueDate)
	f.run(t)
	matchID := f.matchIDForExternal(t, externalID)

	if got := f.breakStatus(t, breakID); got != "in_case" {
		t.Fatalf("break status = %s, want untouched in_case", got)
	}
	afterCase := f.caseRow(t, caseID)
	if afterCase.Status != "resolved" || afterCase.Version != beforeCase.Version ||
		afterCase.ResolutionType == nil || *afterCase.ResolutionType != "manual" {
		t.Fatalf("case after skip = %+v, want unchanged resolved/manual/version %d",
			afterCase, beforeCase.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows = %d, want 0", got)
	}

	skipCount := func() int {
		n := 0
		for _, row := range f.audits(t, caseID) {
			if row.Action == caselifecycle.ActionCaseLateMatchSkipped {
				n++
				if row.ActorType != "system" || row.ActorID != "matcher" {
					t.Fatalf("skip audit actor = %s:%s, want system:matcher",
						row.ActorType, row.ActorID)
				}
				after := jsonObject(t, row.After)
				if after["match_id"] != matchID || after["reason"] != caselifecycle.SkipResolvedOtherResolution {
					t.Fatalf("skip audit after = %v, want match/reason", after)
				}
			}
		}
		return n
	}
	if got := skipCount(); got != 1 {
		t.Fatalf("late-match skip audits = %d, want exactly 1", got)
	}

	// A direct replay finds the recorded skip for this (case, match) and writes
	// no duplicate audit.
	if _, err := f.reconcile(context.Background(), matchID, "system:matcher"); err != nil {
		t.Fatalf("replay ReconcileLateMatch: %v", err)
	}
	if got := skipCount(); got != 1 {
		t.Fatalf("late-match skip audits after replay = %d, want still 1", got)
	}
}

// TestLateMatchMatcherRollbackOnLifecycleFailure injects a failure on the case
// transition audit, which happens after the break supersession, proposal
// transition, and approval.cancel insert. The matcher transaction must roll
// back every row, and a retry after the fault is removed must succeed.
func TestLateMatchMatcherRollbackOnLifecycleFailure(t *testing.T) {
	f, externalID, caseID, breakID, proposalID := setupLateCase(t)
	f.addLedger(t, "LATE-CASE", -10_000, lateValueDate)
	ctx := context.Background()
	beforeAudits := f.count(t, `SELECT count(*) FROM audit_log`)
	beforeRuns := f.count(t, `SELECT count(*) FROM match_runs`)

	if _, err := f.db.Pool.Exec(ctx, `
		CREATE FUNCTION public.reject_late_match_case_transition() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.action = 'case.transition' THEN
				RAISE EXCEPTION 'injected late-match lifecycle failure';
			END IF;
			RETURN NEW;
		END
		$$`); err != nil {
		t.Fatalf("create rollback trigger function: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx, `
		CREATE TRIGGER reject_late_match_case_transition
		BEFORE INSERT ON audit_log
		FOR EACH ROW EXECUTE FUNCTION public.reject_late_match_case_transition()`); err != nil {
		t.Fatalf("create rollback trigger: %v", err)
	}

	err := f.runErr(t)
	if err == nil {
		t.Fatal("matcher run succeeded with the late-match case transition rejected")
	}
	if !strings.Contains(err.Error(), "injected late-match lifecycle failure") {
		t.Fatalf("error = %v, want the injected lifecycle failure", err)
	}

	if got := f.count(t, `SELECT count(*) FROM matches WHERE external_record_id = $1::uuid`, externalID); got != 0 {
		t.Fatalf("matches after rollback = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM match_legs`); got != 0 {
		t.Fatalf("match legs after rollback = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM match_runs`); got != beforeRuns {
		t.Fatalf("match runs after rollback = %d, want %d", got, beforeRuns)
	}
	if got := f.breakStatus(t, breakID); got != "in_case" {
		t.Fatalf("break status after rollback = %s, want in_case", got)
	}
	if got := f.proposal(t, proposalID); got.Status != "proposed" || got.Version != 0 {
		t.Fatalf("proposal after rollback = %s/version %d, want proposed/0", got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "proposal_pending" || got.Version != 2 {
		t.Fatalf("case after rollback = %s/version %d, want proposal_pending/2",
			got.Status, got.Version)
	}
	if got := len(f.outbox(t)); got != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0", got)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != beforeAudits {
		t.Fatalf("audit rows after rollback = %d, want %d", got, beforeAudits)
	}

	if _, err := f.db.Pool.Exec(ctx, `DROP TRIGGER reject_late_match_case_transition ON audit_log`); err != nil {
		t.Fatalf("drop rollback trigger: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx, `DROP FUNCTION public.reject_late_match_case_transition()`); err != nil {
		t.Fatalf("drop rollback trigger function: %v", err)
	}

	f.run(t)
	if got := f.breakStatus(t, breakID); got != "superseded" {
		t.Fatalf("break status after retry = %s, want superseded", got)
	}
	if got := f.proposal(t, proposalID); got.Status != "superseded" || got.Version != 1 {
		t.Fatalf("proposal after retry = %s/version %d, want superseded/1", got.Status, got.Version)
	}
	if got := f.caseRow(t, caseID); got.Status != "resolved" {
		t.Fatalf("case after retry = %s, want resolved", got.Status)
	}
	if got := len(f.outbox(t)); got != 1 {
		t.Fatalf("outbox rows after retry = %d, want 1", got)
	}
}

// TestReconcileLateMatchMissingAndUnmatched covers the typed-error/no-op
// contract for a missing match and an unmatched historical match.
func TestReconcileLateMatchMissingAndUnmatched(t *testing.T) {
	f := newLateMatcherFixture(t)
	ctx := context.Background()
	if _, err := f.reconcile(ctx, "00000000-0000-4000-8000-000000000000", "system:matcher"); !errors.Is(err, caselifecycle.ErrMatchNotFound) {
		t.Fatalf("missing match error = %v, want ErrMatchNotFound", err)
	}

	externalID, _ := f.addExternal(t, "LATE-UNMATCH", -5_000, lateValueDate)
	f.addLedger(t, "LATE-UNMATCH", -5_000, lateValueDate)
	f.run(t)
	matchID := f.matchIDForExternal(t, externalID)
	if _, err := f.db.Pool.Exec(ctx,
		`UPDATE matches SET status = 'unmatched', unmatched_at = $2
		  WHERE id = $1::uuid`, matchID, f.clock.Now()); err != nil {
		t.Fatalf("mark match unmatched: %v", err)
	}
	beforeAudits := f.count(t, `SELECT count(*) FROM audit_log`)

	result, err := f.reconcile(ctx, matchID, "system:matcher")
	if err != nil {
		t.Fatalf("ReconcileLateMatch on unmatched match: %v", err)
	}
	if result.Reconciled || len(result.Cases) != 0 {
		t.Fatalf("unmatched result = %+v, want a no-op", result)
	}
	if got := f.count(t, `SELECT count(*) FROM audit_log`); got != beforeAudits {
		t.Fatalf("audit rows after unmatched no-op = %d, want %d", got, beforeAudits)
	}
}

// TestBreakMatchLinkageBothBranches exercises the no-breaks.match_id linkage:
// a break is consumed by ledger overlap or by JSONB containment of the match's
// external ref, and never merely by sharing one of those identifiers across a
// different scope. The match is created by the real matcher.
func TestBreakMatchLinkageBothBranches(t *testing.T) {
	f := newLateMatcherFixture(t)
	externalID, sourceRecordID := f.addExternal(t, "LINK-CASE", -10_000, lateValueDate)
	ledgerID := f.addLedger(t, "LINK-CASE", -10_000, lateValueDate)
	result := f.run(t)
	if result.Stats.Matched != 1 {
		t.Fatalf("matched = %d, want 1", result.Stats.Matched)
	}
	matchID := f.matchIDForExternal(t, externalID)

	otherScopeID := f.insertOtherScope(t)
	ledgerBreak := f.insertBreak(t, f.scopeID, 1, result.RunID, []string{ledgerID}, nil)
	externalBreak := f.insertBreak(t, f.scopeID, 2, result.RunID, nil,
		[]match.RecordRef{{Source: f.source, SourceRecordID: sourceRecordID}})
	_ = f.insertBreak(t, otherScopeID, 3, result.RunID, []string{ledgerID},
		[]match.RecordRef{{Source: f.source, SourceRecordID: sourceRecordID}})
	_ = f.insertBreak(t, f.scopeID, 4, result.RunID,
		[]string{"99999999-9999-4999-8999-999999999999"},
		[]match.RecordRef{{Source: f.source, SourceRecordID: "other-record"}})

	var got []string
	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = caselifecycle.LinkedBreakIDsForTest(ctx, tx, matchID)
			return err
		})
	if err != nil {
		t.Fatalf("linked breaks: %v", err)
	}
	want := []string{externalBreak, ledgerBreak}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("linked break ids = %v, want %v (other scope and unrelated identifiers must not link)",
			got, want)
	}
}

func (f *lateMatcherFixture) insertOtherScope(t *testing.T) string {
	t.Helper()
	accountID := insertLateAccount(t, f.fixture, "late_other_clearing", "asset")
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'late_other_source', 'USD') RETURNING id::text`,
		accountID).Scan(&id); err != nil {
		t.Fatalf("insert other reconciliation scope: %v", err)
	}
	return id
}

func (f *lateMatcherFixture) insertBreak(t *testing.T, scopeID string, seq int,
	matchRunID string, ledgerTxnIDs []string, refs []match.RecordRef) string {
	t.Helper()
	if ledgerTxnIDs == nil {
		ledgerTxnIDs = []string{}
	}
	if refs == nil {
		refs = []match.RecordRef{}
	}
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("encode break refs: %v", err)
	}
	side := "external_only"
	if len(ledgerTxnIDs) > 0 {
		side = "ledger_only"
	}
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, $5::text[]::uuid[],
		         $6::jsonb, 0, 'USD', 'open', $7)
		 RETURNING id::text`,
		fmt.Sprintf("%064x", seq), scopeID, matchRunID, side, ledgerTxnIDs,
		string(refsJSON), f.clock.Now()).Scan(&id); err != nil {
		t.Fatalf("insert linkage break %d: %v", seq, err)
	}
	return id
}
