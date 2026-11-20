//go:build integration

package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/slack"
	approvalstate "github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// sourceFixture seeds the proposal/case/break graph the read model hydrates.
// It reuses the package TestMain in store_integration_test.go.
type sourceFixture struct {
	t       *testing.T
	d       *testdb.DB
	app     *pgxpool.Pool
	scopeID string
	runID   string
	seq     atomic.Int64
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	d := testdb.New(t)
	role, err := db.OpenRole(context.Background(), db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(role.Close)
	f := &sourceFixture{t: t, d: d, app: role.Pool}

	ctx := context.Background()
	var accountID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency)
		 VALUES ($1, 'Slack source test account', 'asset', 'USD')
		 RETURNING id::text`, "SLACK-SRC-"+t.Name()).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, 'psp_slack_src', 'USD') RETURNING id::text`,
		accountID).Scan(&f.scopeID); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'slack-source-test')
		 RETURNING id::text`, f.scopeID, at).Scan(&f.runID); err != nil {
		t.Fatalf("seed match run: %v", err)
	}
	return f
}

func (f *sourceFixture) newCase(priority int, valueMinor int64, currency string, createdAt time.Time) string {
	f.t.Helper()
	var caseID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES ($1, $2, $3, 'proposal_pending', 1, 0, $4)
		 RETURNING id::text`,
		priority, valueMinor, currency, createdAt).Scan(&caseID); err != nil {
		f.t.Fatalf("seed case: %v", err)
	}
	return caseID
}

func (f *sourceFixture) newRun(caseID string) string {
	f.t.Helper()
	var runID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		f.t.Fatalf("seed agent run: %v", err)
	}
	return runID
}

func (f *sourceFixture) attachBreak(caseID, side string, deltaMinor int64, currency string, linkedAt time.Time) string {
	f.t.Helper()
	seq := f.seq.Add(1)
	signature := fmt.Sprintf("%064x", seq)
	refs := fmt.Sprintf(
		`[{"source":"psp_slack_src","source_record_id":"ref-%d"}]`, seq)
	var breakID string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, '{}'::uuid[],
		         $5::jsonb, $6, $7, 'in_case', $8)
		 RETURNING id::text`,
		signature, f.scopeID, f.runID, side, refs, deltaMinor, currency, linkedAt).Scan(&breakID); err != nil {
		f.t.Fatalf("seed break: %v", err)
	}
	if _, err := f.d.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, caseID, breakID, linkedAt); err != nil {
		f.t.Fatalf("link case break: %v", err)
	}
	return breakID
}

type sourceProposalSeed struct {
	caseID          string
	typ             string
	status          string
	version         int
	riskTier        any
	patternClaimed  any
	patternVerified any
	verification    any
	policyDecision  any
	rationale       string
	evidenceJSON    string
	expiresAt       any
	createdAt       time.Time
}

func (f *sourceFixture) newProposal(seed sourceProposalSeed) string {
	f.t.Helper()
	runID := f.newRun(seed.caseID)
	seq := f.seq.Add(1)
	if seed.typ == "" {
		seed.typ = "write_off"
	}
	if seed.status == "" {
		seed.status = string(approvalstate.StatusPendingApproval)
	}
	if seed.version == 0 {
		seed.version = 3
	}
	if seed.rationale == "" {
		seed.rationale = "The fee matches the schedule."
	}
	if seed.evidenceJSON == "" {
		seed.evidenceJSON = `["ext:bank:abc123"]`
	}
	if seed.createdAt.IsZero() {
		seed.createdAt = time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	}
	var proposalID string
	err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence, confidence,
		      pattern_claimed, pattern_verified, verification, risk_tier,
		      policy_decision, status, version, state_hash, idempotency_key,
		      created_at, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb, $4, $5::jsonb, 0.9000,
		         $6, $7, $8::jsonb, $9, $10::jsonb, $11, $12, 'state-hash', $13,
		         $14, $15)
		 RETURNING id::text`,
		seed.caseID, runID, seed.typ, seed.rationale, seed.evidenceJSON,
		seed.patternClaimed, seed.patternVerified, seed.verification, seed.riskTier,
		seed.policyDecision, seed.status, seed.version,
		fmt.Sprintf("m4-12-source-%d-%d", os.Getpid(), seq),
		seed.createdAt, seed.expiresAt).Scan(&proposalID)
	if err != nil {
		f.t.Fatalf("seed proposal: %v", err)
	}
	return proposalID
}

func TestSourceHydratesApprovalMessage(t *testing.T) {
	f := newSourceFixture(t)
	source, err := slack.NewSource(f.app)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	caseCreated := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	caseID := f.newCase(2, 123456, "USD", caseCreated)
	f.attachBreak(caseID, "external_only", 1250, "USD", caseCreated.Add(time.Minute))
	f.attachBreak(caseID, "amount_mismatch", 99, "EUR", caseCreated.Add(2*time.Minute))

	verification := `{"reason":"within fee schedule","expected_minor":11250,"actual_minor":12500,"currency":"USD"}`
	policy := `{"schema_version":1,"tier":"T1","rule":"high_value","reasons":["high_value"],"config_hash":"` + strings.Repeat("a", 64) + `"}`
	expires := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	proposalID := f.newProposal(sourceProposalSeed{
		caseID:          caseID,
		typ:             "write_off",
		riskTier:        "T1",
		patternClaimed:  "fee_variance",
		patternVerified: "fee_variance",
		verification:    verification,
		policyDecision:  policy,
		rationale:       "The bank fee is 12.50 USD higher than the schedule.",
		evidenceJSON:    `["ext:bank:abc123","ledger_txn:11111111-2222-3333-4444-555555555555"]`,
		expiresAt:       expires,
	})

	data, err := source.ApprovalMessage(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("ApprovalMessage: %v", err)
	}
	if data.ProposalID != proposalID || data.ProposalType != "write_off" ||
		data.ProposalVersion != 3 || data.ProposalStatus != "pending_approval" ||
		data.RiskTier != "T1" {
		t.Fatalf("proposal fields = %+v", data)
	}
	if !data.ExpiresAt.Equal(expires) {
		t.Fatalf("expires_at = %s, want %s", data.ExpiresAt, expires)
	}
	if data.CaseID != caseID || data.CaseValueMinor != 123456 ||
		data.CaseCurrency != "USD" || data.CasePriority != 2 ||
		!data.CaseCreatedAt.Equal(caseCreated) {
		t.Fatalf("case fields = %+v", data)
	}
	if data.BreakCount != 2 || data.PrimaryBreakSide != "external_only" ||
		data.PrimaryBreakDeltaMinor != 1250 || data.PrimaryBreakCurrency != "USD" {
		t.Fatalf("break fields = count %d side %q delta %d currency %q",
			data.BreakCount, data.PrimaryBreakSide,
			data.PrimaryBreakDeltaMinor, data.PrimaryBreakCurrency)
	}
	if data.PatternClaimed != "fee_variance" || data.PatternVerified != "fee_variance" {
		t.Fatalf("pattern fields = %q/%q", data.PatternClaimed, data.PatternVerified)
	}
	if data.PolicyTier != "T1" || data.PolicyRule != "high_value" {
		t.Fatalf("policy fields = %q/%q", data.PolicyTier, data.PolicyRule)
	}
	if data.Rationale != "The bank fee is 12.50 USD higher than the schedule." {
		t.Fatalf("rationale = %q", data.Rationale)
	}
	if len(data.Evidence) != 2 || data.Evidence[0] != "ext:bank:abc123" ||
		data.Evidence[1] != "ledger_txn:11111111-2222-3333-4444-555555555555" {
		t.Fatalf("evidence = %#v", data.Evidence)
	}
	var gotVerification map[string]any
	if err := json.Unmarshal(data.Verification, &gotVerification); err != nil {
		t.Fatalf("verification json: %v", err)
	}
	if gotVerification["reason"] != "within fee schedule" {
		t.Fatalf("verification = %#v", gotVerification)
	}
	if data.CaseFileURL != "" {
		t.Fatalf("source populated CaseFileURL %q; the server owns the URL", data.CaseFileURL)
	}
}

func TestSourceDefaultsWithoutBreaksVerificationOrPolicy(t *testing.T) {
	f := newSourceFixture(t)
	source, err := slack.NewSource(f.app)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	caseID := f.newCase(0, 500, "USD", time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	proposalID := f.newProposal(sourceProposalSeed{
		caseID:         caseID,
		typ:            "adjustment",
		evidenceJSON:   `[]`,
		patternClaimed: nil,
	})

	data, err := source.ApprovalMessage(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("ApprovalMessage: %v", err)
	}
	if data.BreakCount != 0 || data.PrimaryBreakSide != "" ||
		data.PrimaryBreakDeltaMinor != 0 || data.PrimaryBreakCurrency != "" {
		t.Fatalf("break defaults = %+v", data)
	}
	if data.PatternClaimed != "" || data.PatternVerified != "" ||
		len(data.Verification) != 0 || data.PolicyRule != "" || data.PolicyTier != "" {
		t.Fatalf("verifier/policy defaults = %+v", data)
	}
	if !data.ExpiresAt.IsZero() {
		t.Fatalf("expires_at should be zero, got %s", data.ExpiresAt)
	}
	if data.Evidence == nil || len(data.Evidence) != 0 {
		t.Fatalf("empty evidence = %#v, want an empty list", data.Evidence)
	}
}

func TestSourceNotFoundAndValidation(t *testing.T) {
	f := newSourceFixture(t)
	source, err := slack.NewSource(f.app)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	ctx := context.Background()

	missing := "99999999-8888-7777-6666-555555555555"
	if _, err := source.ApprovalMessage(ctx, missing); !errors.Is(err, approval.ErrProposalNotFound) {
		t.Fatalf("missing proposal error = %v, want ErrProposalNotFound", err)
	}
	for _, id := range []string{"", "not-a-uuid", "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"} {
		if _, err := source.ApprovalMessage(ctx, id); !errors.Is(err, slack.ErrInvalidMessage) {
			t.Fatalf("invalid id %q error = %v, want ErrInvalidMessage", id, err)
		}
	}
	var typedNil *pgxpool.Pool
	if _, err := slack.NewSource(typedNil); !errors.Is(err, slack.ErrInvalidMessage) {
		t.Fatalf("NewSource(typed nil) error = %v, want ErrInvalidMessage", err)
	}
	var nilSource *slack.DBSource
	if _, err := nilSource.ApprovalMessage(ctx, missing); !errors.Is(err, slack.ErrInvalidMessage) {
		t.Fatalf("nil source error = %v, want ErrInvalidMessage", err)
	}
}

func TestSourceApproverDisplayName(t *testing.T) {
	f := newSourceFixture(t)
	source, err := slack.NewSource(f.app)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	ctx := context.Background()
	var approverID string
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO approvers (display_name, role) VALUES ('Ada Lovelace', 'approver')
		 RETURNING id::text`).Scan(&approverID); err != nil {
		t.Fatalf("seed approver: %v", err)
	}
	name, err := source.ApproverDisplayName(ctx, approverID)
	if err != nil {
		t.Fatalf("ApproverDisplayName: %v", err)
	}
	if name != "Ada Lovelace" {
		t.Fatalf("display name = %q", name)
	}
	missing := "99999999-8888-7777-6666-555555555555"
	if _, err := source.ApproverDisplayName(ctx, missing); !errors.Is(err, approval.ErrApproverNotFound) {
		t.Fatalf("missing approver error = %v, want ErrApproverNotFound", err)
	}
	if _, err := source.ApproverDisplayName(ctx, "agent:bot"); !errors.Is(err, slack.ErrInvalidMessage) {
		t.Fatalf("invalid approver error = %v, want ErrInvalidMessage", err)
	}
}
