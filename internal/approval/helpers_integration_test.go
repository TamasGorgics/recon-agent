//go:build integration

package approval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "approval: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "approval: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

const (
	testTTL = 48 * time.Hour
	testNow = "2026-11-17T12:00:00Z"
)

var proposalCounter atomic.Int64

type fakeEngine struct {
	decision     policy.Decision
	ttl          time.Duration
	calls        int
	proposal     policy.Proposal
	verification policy.Verification
	facts        policy.Facts
}

func (f *fakeEngine) Evaluate(p policy.Proposal, v policy.Verification, facts policy.Facts) policy.Decision {
	f.calls++
	f.proposal = p
	f.verification = v
	f.facts = facts
	return f.decision
}

func (f *fakeEngine) ProposalTTL() time.Duration { return f.ttl }

type fakeVerifier struct {
	verification policy.Verification
	err          error
	calls        int
	proposalIDs  []string
	onVerify     func(ctx context.Context, proposalID string)
}

func (v *fakeVerifier) Verify(ctx context.Context, proposalID string) (policy.Verification, error) {
	v.calls++
	v.proposalIDs = append(v.proposalIDs, proposalID)
	if v.err != nil {
		return policy.Verification{}, v.err
	}
	if v.onVerify != nil {
		v.onVerify(ctx, proposalID)
	}
	return v.verification, nil
}

type fakeLimits struct {
	result     limits.Result
	err        error
	calls      int
	candidates []limits.Candidate
}

func (l *fakeLimits) Check(ctx context.Context, c limits.Candidate) (limits.Result, error) {
	l.calls++
	l.candidates = append(l.candidates, c)
	if l.err != nil {
		return limits.Result{}, l.err
	}
	return l.result, nil
}

type fixture struct {
	t          *testing.T
	d          *testdb.DB
	app        *pgxpool.Pool
	clock      *clock.Fake
	state      *state.Service
	loaded     policy.LoadedConfig
	engine     *fakeEngine
	verifier   *fakeVerifier
	limits     *fakeLimits
	service    *approval.Service
	testNowUTC time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)
	fakeClock := clock.NewFake(mustParseTime(t, testNow))
	stateService, err := state.NewService(app, fakeClock)
	if err != nil {
		t.Fatalf("state.NewService: %v", err)
	}
	loaded, err := policy.LoadFile("../../policy.yaml")
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}

	engine := &fakeEngine{
		decision: decisionForTier(t, loaded, policy.TierT1),
		ttl:      testTTL,
	}
	verifier := &fakeVerifier{
		verification: policy.Verification{
			Pattern: policy.PatternFeeVariance,
			Passed:  true,
			Details: json.RawMessage(`{"schema_version":1,"reason":""}`),
		},
	}
	limitsFake := &fakeLimits{
		result: limits.Result{
			Allowed:       true,
			Tier:          policy.TierT0,
			Reason:        limits.ReasonWithinLimits,
			SwitchEnabled: true,
		},
	}
	service, err := approval.NewService(approval.Options{
		Pool:     app,
		Clock:    fakeClock,
		State:    stateService,
		Verifier: verifier,
		Limits:   limitsFake,
		Policy:   approval.Policy{Engine: engine, Loaded: loaded},
		TTL:      testTTL,
	})
	if err != nil {
		t.Fatalf("approval.NewService: %v", err)
	}
	f := &fixture{
		t:          t,
		d:          d,
		app:        app,
		clock:      fakeClock,
		state:      stateService,
		loaded:     loaded,
		engine:     engine,
		verifier:   verifier,
		limits:     limitsFake,
		service:    service,
		testNowUTC: mustParseTime(t, testNow),
	}
	// The M2 seed is auto_apply_enabled=false; most Request tests exercise the
	// enabled path and the disabled test flips it back explicitly.
	f.setAutoApply(true)
	return f
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	rp, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(rp.Close)
	return rp.Pool
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func decisionForTier(t *testing.T, loaded policy.LoadedConfig, tier policy.Tier) policy.Decision {
	t.Helper()
	decision := policy.Decision{
		SchemaVersion: policy.SchemaVersion,
		Tier:          tier,
		ConfigHash:    loaded.Hash,
	}
	switch tier {
	case policy.TierT0:
		decision.Rule = policy.CodeAutoApply
		decision.Reasons = []string{policy.CodeAutoApply}
	case policy.TierT1:
		decision.Rule = policy.CodeVerifierFailed
		decision.Reasons = []string{policy.CodeVerifierFailed}
	case policy.TierT2:
		decision.Rule = policy.CodeHighValue
		decision.Reasons = []string{policy.CodeHighValue}
	default:
		t.Fatalf("decisionForTier: unknown tier %q", tier)
	}
	if err := decision.Validate(); err != nil {
		t.Fatalf("test decision for %s: %v", tier, err)
	}
	return decision
}

type proposalSpec struct {
	Type            string
	Payload         map[string]any
	CaseCurrency    string
	CaseStatus      string
	Status          string
	PatternVerified *string
	PatternClaimed  *string
	Confidence      string
	Evidence        []string
}

func (f *fixture) seedProposal(spec proposalSpec) (proposalID, caseID, runID string) {
	f.t.Helper()
	if spec.CaseCurrency == "" {
		spec.CaseCurrency = "USD"
	}
	if spec.Status == "" {
		spec.Status = "proposed"
	}
	if spec.CaseStatus == "" {
		spec.CaseStatus = "open"
	}
	if spec.Confidence == "" {
		spec.Confidence = "0.9000"
	}
	if spec.Evidence == nil {
		spec.Evidence = []string{"ext:feed:record-1"}
	}
	payload, err := json.Marshal(spec.Payload)
	if err != nil {
		f.t.Fatalf("marshal payload: %v", err)
	}
	evidence, err := json.Marshal(spec.Evidence)
	if err != nil {
		f.t.Fatalf("marshal evidence: %v", err)
	}

	ctx := context.Background()
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, created_at)
		 VALUES (0, 100, $1, $2, 0, now())
		 RETURNING id::text`, spec.CaseCurrency, spec.CaseStatus).Scan(&caseID); err != nil {
		f.t.Fatalf("seed case: %v", err)
	}
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO agent_runs (case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, 'scripted', 'prompt-v1', now())
		 RETURNING id::text`, caseID).Scan(&runID); err != nil {
		f.t.Fatalf("seed agent run: %v", err)
	}
	key := fmt.Sprintf("m4-11-request-%d", proposalCounter.Add(1))
	if err := f.d.Pool.QueryRow(ctx,
		`INSERT INTO proposals
		     (case_id, agent_run_id, type, payload, rationale, evidence,
		      confidence, pattern_claimed, pattern_verified, status, version,
		      state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4::jsonb, 'seed rationale', $5::jsonb,
		         $6::numeric, $7, $8, $9, 0, 'state-hash', $10, now())
		 RETURNING id::text`,
		caseID, runID, spec.Type, string(payload), string(evidence),
		spec.Confidence, spec.PatternClaimed, spec.PatternVerified, spec.Status, key).
		Scan(&proposalID); err != nil {
		f.t.Fatalf("seed proposal: %v", err)
	}
	return proposalID, caseID, runID
}

func adjustmentPayload(amount int64) map[string]any {
	return map[string]any{
		"lines": []map[string]any{
			{
				"account_code": "clearing_suspense",
				"direction":    "debit",
				"amount_minor": amount,
				"currency":     "USD",
			},
			{
				"account_code": "fees_expense",
				"direction":    "credit",
				"amount_minor": amount,
				"currency":     "USD",
			},
		},
	}
}

func linkPayload() map[string]any {
	return map[string]any{
		"external_ref":  "ext:feed:record-1",
		"ledger_txn_id": "00000000-0000-4000-8000-000000000001",
	}
}

func strPtr(value string) *string { return &value }

type storedProposal struct {
	Status     string
	Version    int
	Tier       *string
	Decision   []byte
	ExpiresAt  *time.Time
	CaseID     string
	Verified   *string
	Confidence string
}

func (f *fixture) readProposal(proposalID string) storedProposal {
	f.t.Helper()
	var stored storedProposal
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status, version, risk_tier, policy_decision, expires_at,
		        case_id::text, pattern_verified, confidence::text
		   FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&stored.Status, &stored.Version, &stored.Tier, &stored.Decision,
			&stored.ExpiresAt, &stored.CaseID, &stored.Verified, &stored.Confidence); err != nil {
		f.t.Fatalf("read proposal %s: %v", proposalID, err)
	}
	return stored
}

func (f *fixture) decisionOf(stored storedProposal) policy.Decision {
	f.t.Helper()
	var decision policy.Decision
	if len(stored.Decision) == 0 {
		f.t.Fatal("proposal has no policy_decision")
	}
	if err := json.Unmarshal(stored.Decision, &decision); err != nil {
		f.t.Fatalf("decode policy decision: %v", err)
	}
	return decision
}

type auditRow struct {
	Action    string
	ActorType string
	ActorID   string
	Before    []byte
	After     []byte
}

func (f *fixture) audits(entityID string) []auditRow {
	f.t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT action, actor_type, actor_id, before, after
		   FROM audit_log
		  WHERE entity_type = 'proposal' AND entity_id = $1
		  ORDER BY seq`, entityID)
	if err != nil {
		f.t.Fatalf("query audits for %s: %v", entityID, err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.ActorType, &row.ActorID, &row.Before, &row.After); err != nil {
			f.t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate audits: %v", err)
	}
	return out
}

func (f *fixture) auditByAction(entityID, action string) (auditRow, bool) {
	f.t.Helper()
	for _, row := range f.audits(entityID) {
		if row.Action == action {
			return row, true
		}
	}
	return auditRow{}, false
}

type outboxRow struct {
	AggregateType  string
	AggregateID    string
	Topic          string
	Payload        []byte
	IdempotencyKey string
}

func (f *fixture) outboxRows(proposalID string) []outboxRow {
	f.t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT aggregate_type, aggregate_id, topic, payload, idempotency_key
		   FROM outbox WHERE aggregate_id = $1`, proposalID)
	if err != nil {
		f.t.Fatalf("query outbox for %s: %v", proposalID, err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.AggregateType, &row.AggregateID, &row.Topic,
			&row.Payload, &row.IdempotencyKey); err != nil {
			f.t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

func (f *fixture) setAutoApply(enabled bool) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE system_switches SET enabled = $2 WHERE name = $1`,
		limits.SwitchAutoApply, enabled); err != nil {
		f.t.Fatalf("set auto-apply switch: %v", err)
	}
}

type approverSpec struct {
	DisplayName string
	Role        string
	Active      *bool
	APIKeyID    *string
	SlackUserID *string
}

// seedApprover inserts an approvers row with the admin/owner test pool. The
// approvers table stays read-only for recon_app (M4-01), so tests cannot create
// identities through the service pool.
func (f *fixture) seedApprover(spec approverSpec) (approverID, apiKeyID string) {
	f.t.Helper()
	if spec.DisplayName == "" {
		spec.DisplayName = "M4-11 Approver"
	}
	if spec.Role == "" {
		spec.Role = "approver"
	}
	active := true
	if spec.Active != nil {
		active = *spec.Active
	}
	var storedKey *string
	if err := f.d.Pool.QueryRow(context.Background(),
		`INSERT INTO approvers (display_name, role, active, api_key_id, slack_user_id)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text, api_key_id`,
		spec.DisplayName, spec.Role, active, spec.APIKeyID, spec.SlackUserID).
		Scan(&approverID, &storedKey); err != nil {
		f.t.Fatalf("seed approver: %v", err)
	}
	if storedKey != nil {
		apiKeyID = *storedKey
	}
	return approverID, apiKeyID
}

type approvalRow struct {
	Channel        string
	ExternalRef    *string
	ApproverID     string
	Decision       string
	DecidedAt      time.Time
	SignatureValid bool
}

func (f *fixture) approvalRows(proposalID string) []approvalRow {
	f.t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT channel, external_ref, approver_id::text, decision,
		        decided_at, signature_valid
		   FROM approvals
		  WHERE proposal_id = $1::uuid
		  ORDER BY decided_at, approver_id`, proposalID)
	if err != nil {
		f.t.Fatalf("query approvals for %s: %v", proposalID, err)
	}
	defer rows.Close()
	var out []approvalRow
	for rows.Next() {
		var row approvalRow
		if err := rows.Scan(&row.Channel, &row.ExternalRef, &row.ApproverID,
			&row.Decision, &row.DecidedAt, &row.SignatureValid); err != nil {
			f.t.Fatalf("scan approval row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate approvals: %v", err)
	}
	return out
}

// appendHumanAudit writes a real hash-chained human audit row on an arbitrary
// entity. Tests use it to model the maker actions four-eyes must detect.
func (f *fixture) appendHumanAudit(entityType, entityID, actorID, action string) {
	f.t.Helper()
	actor, err := audit.HumanActor(actorID)
	if err != nil {
		f.t.Fatalf("human actor %q: %v", actorID, err)
	}
	err = db.WithTx(context.Background(), f.d.Pool, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := audit.Append(ctx, tx, audit.Entry{
				Actor:      actor,
				Action:     action,
				EntityType: entityType,
				EntityID:   entityID,
				Before:     map[string]any{"seed": "co-maker"},
				After:      map[string]any{"seed": "co-maker"},
				Timestamp:  f.clock.Now().UTC(),
			})
			return err
		})
	if err != nil {
		f.t.Fatalf("append human audit on %s/%s: %v", entityType, entityID, err)
	}
}

// readCaseStatus returns the case status so Decide tests can prove the decision
// path does not mutate the case lifecycle (M4-10 owns that).
func (f *fixture) readCaseStatus(caseID string) string {
	f.t.Helper()
	var status string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status FROM cases WHERE id = $1::uuid`, caseID).Scan(&status); err != nil {
		f.t.Fatalf("read case %s: %v", caseID, err)
	}
	return status
}

type storedCase struct {
	Status           string
	Version          int
	Attempts         int
	EscalationReason *string
}

// readCase reads the lifecycle columns Decide's hook may change.
func (f *fixture) readCase(caseID string) storedCase {
	f.t.Helper()
	var stored storedCase
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status, version, attempts, escalation_reason
		   FROM cases WHERE id = $1::uuid`, caseID).
		Scan(&stored.Status, &stored.Version, &stored.Attempts,
			&stored.EscalationReason); err != nil {
		f.t.Fatalf("read case %s: %v", caseID, err)
	}
	return stored
}

// caseAudits returns the case-entity audit rows the M4-10 hook writes.
func (f *fixture) caseAudits(caseID string) []auditRow {
	f.t.Helper()
	rows, err := f.d.Pool.Query(context.Background(),
		`SELECT action, actor_type, actor_id, before, after
		   FROM audit_log
		  WHERE entity_type = 'case' AND entity_id = $1
		  ORDER BY seq`, caseID)
	if err != nil {
		f.t.Fatalf("query case audits for %s: %v", caseID, err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.ActorType, &row.ActorID,
			&row.Before, &row.After); err != nil {
			f.t.Fatalf("scan case audit row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate case audits: %v", err)
	}
	return out
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("expected a JSON object, got NULL")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode JSON object %s: %v", raw, err)
	}
	return object
}
