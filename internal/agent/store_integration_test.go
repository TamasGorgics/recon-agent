//go:build integration

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func openAgentRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping %s pool: %v", role, err)
	}
	return pool
}

func insertAgentCase(t *testing.T, d *testdb.DB, status string, leasedBy *string, leaseExpires *time.Time) string {
	t.Helper()
	var caseID string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO cases
		     (priority, value_minor, currency, created_at, status, leased_by, lease_expires_at)
		 VALUES (2, 1250, 'USD', '2026-01-01T00:00:00Z', $1, $2, $3)
		 RETURNING id::text`, status, leasedBy, leaseExpires).Scan(&caseID); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	return caseID
}

func TestAgentDBStoreIntegration(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openAgentRolePool(t, d, db.RoleApp)
	agentRO := openAgentRolePool(t, d, db.RoleAgentRO)

	caseID := insertAgentCase(t, d, "open", nil, nil)
	runID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	store := agent.NewDBStore(app)
	startedAt := time.Date(2026, 2, 3, 4, 5, 6, 123456000, time.UTC)

	if err := store.CreateRun(ctx, agent.RunRecord{
		ID: runID, CaseID: caseID, Model: "test-model", PromptVersion: "v1", StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	response := json.RawMessage(`{"id":"resp_1","actual_model":"test-model-2026"}`)
	usage := json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5}`)
	if err := store.RecordLLMCall(ctx, agent.LLMCall{
		RunID: runID, Seq: 1, Model: "test-model-2026", RequestHash: "hash-1",
		Response: response, Usage: usage, StartedAt: startedAt, DurationMS: 12,
	}); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}

	if _, err := store.ProposalForRun(ctx, runID); !errors.Is(err, agent.ErrProposalNotFound) {
		t.Fatalf("ProposalForRun without a proposal = %v, want ErrProposalNotFound", err)
	}

	endedAt := startedAt.Add(2 * time.Second)
	if err := store.FinishRun(ctx, agent.RunFinish{
		RunID: runID, EndedAt: endedAt, TokensIn: 10, TokensOut: 5,
		CostUSD: decimal.RequireFromString("0.25"), Outcome: agent.OutcomeProposal,
		Flags: []string{"injection_suspected", "a"}, Model: "test-model-2026",
	}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	var (
		gotEnded  *time.Time
		tokensIn  int64
		tokensOut int64
		cost      string
		outcome   *string
		flags     string
		model     string
		promptVer string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT ended_at, tokens_in, tokens_out, cost_usd::text, outcome,
		        injection_flags::text, model, prompt_version
		   FROM agent_runs WHERE id = $1::uuid`, runID).Scan(
		&gotEnded, &tokensIn, &tokensOut, &cost, &outcome, &flags, &model, &promptVer); err != nil {
		t.Fatalf("read agent run: %v", err)
	}
	if gotEnded == nil || !gotEnded.Equal(endedAt) {
		t.Fatalf("ended_at = %v, want %v", gotEnded, endedAt)
	}
	if tokensIn != 10 || tokensOut != 5 || cost != "0.250000" {
		t.Fatalf("run usage = %d/%d/%s", tokensIn, tokensOut, cost)
	}
	if outcome == nil || *outcome != string(agent.OutcomeProposal) {
		t.Fatalf("outcome = %v, want proposal", outcome)
	}
	if model != "test-model-2026" || promptVer != "v1" {
		t.Fatalf("model/prompt = %s/%s", model, promptVer)
	}
	var flagList []string
	if err := json.Unmarshal([]byte(flags), &flagList); err != nil {
		t.Fatalf("decode injection_flags: %v", err)
	}
	if len(flagList) != 2 || flagList[0] != "a" || flagList[1] != "injection_suspected" {
		t.Fatalf("injection_flags = %v, want sorted normalized list", flagList)
	}

	var (
		seq         int
		requestHash string
		storedResp  string
		storedUsage string
		duration    int
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT seq, request_hash, response::text, usage::text, duration_ms
		   FROM llm_calls WHERE agent_run_id = $1::uuid`, runID).Scan(
		&seq, &requestHash, &storedResp, &storedUsage, &duration); err != nil {
		t.Fatalf("read llm call: %v", err)
	}
	if seq != 1 || requestHash != "hash-1" || duration != 12 || storedResp == "" || storedUsage == "" {
		t.Fatalf("llm call = seq %d hash %q duration %d", seq, requestHash, duration)
	}

	// Insert the one proposal the run would have written and read it back.
	proposalID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence,
		      confidence, state_hash, idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'adjustment', '{}'::jsonb,
		         'test rationale', '[]'::jsonb, 0.9, 'state-hash', 'idem-key-1', $4)`,
		proposalID, caseID, runID, startedAt); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}
	ref, err := store.ProposalForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ProposalForRun: %v", err)
	}
	if ref.ID != proposalID || ref.Type != "adjustment" || ref.Status != "proposed" {
		t.Fatalf("proposal ref = %+v", ref)
	}

	// Store-side validation never sends malformed rows to Postgres.
	if err := store.CreateRun(ctx, agent.RunRecord{ID: "not-a-uuid", CaseID: caseID, Model: "m", PromptVersion: "v1", StartedAt: startedAt}); err == nil {
		t.Fatal("CreateRun accepted a non-UUID run id")
	}
	if err := store.FinishRun(ctx, agent.RunFinish{RunID: runID, EndedAt: endedAt, Outcome: "aborted"}); err == nil {
		t.Fatal("FinishRun accepted a non-enum outcome")
	}
	if err := store.FinishRun(ctx, agent.RunFinish{
		RunID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", EndedAt: endedAt,
		Outcome: agent.OutcomeEscalated,
	}); !errors.Is(err, agent.ErrRunNotFound) {
		t.Fatalf("FinishRun for a missing run = %v, want ErrRunNotFound", err)
	}

	// The agent-pool role is read-only for run persistence.
	_, err = agentRO.Exec(ctx,
		`INSERT INTO agent_runs (id, case_id, model, prompt_version, started_at)
		 VALUES ('dddddddd-dddd-4ddd-8ddd-dddddddddddd', $1::uuid, 'm', 'v1', now())`, caseID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("agent_ro insert error = %v, want insufficient_privilege (42501)", err)
	}
}

// covers: FIN-24, FIN-30, FIN-33
func TestAgentLoopIntegrationEscalation(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	app := openAgentRolePool(t, d, db.RoleApp)
	agentRO := openAgentRolePool(t, d, db.RoleAgentRO)

	owner := "it-worker"
	expires := time.Date(2026, 2, 3, 4, 10, 6, 0, time.UTC)
	caseID := insertAgentCase(t, d, "in_agent", &owner, &expires)

	runID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	startedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	fakeClock := clock.NewFake(startedAt)

	cfg := config.Config{
		LLM:    config.DefaultLLMConfig(),
		Agent:  config.DefaultAgentConfig(),
		Ledger: config.DefaultLedgerConfig(),
	}
	cfg.Agent.Model = "test-model"

	escalateArgs := `{"case_id":"` + caseID + `","reason":"ambiguous_evidence","summary":"needs an operator"}`
	scripted := llm.NewScripted(llm.ScriptedStep{Response: llm.Response{
		Provider:    llm.ProviderOpenCodeGo,
		ActualModel: "test-model-2026",
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call_1", Name: "escalate", Arguments: json.RawMessage(escalateArgs),
		}}},
		StopReason: llm.StopToolCalls,
		Usage:      llm.Usage{PromptTokens: 10, CompletionTokens: 5, CostUSD: decimal.RequireFromString("0.02")},
	}})

	runner, err := agent.NewProductionRunner(agent.SetupDeps{
		App:       app,
		Agent:     agentRO,
		AgentExec: app,
		Client:    scripted,
		Clock:     fakeClock,
		Config:    cfg,
		NewRunID:  func(string) (string, error) { return runID, nil },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewProductionRunner: %v", err)
	}

	result, err := runner.Run(ctx, caseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeEscalated || result.Escalation == nil ||
		result.Escalation.Reason != "ambiguous_evidence" {
		t.Fatalf("result = %+v", result)
	}
	if result.CaseVersion != 1 || result.LeaseLost {
		t.Fatalf("case version/lease = %d/%v, want 1/false", result.CaseVersion, result.LeaseLost)
	}
	if got := scripted.Calls(); got != 1 {
		t.Fatalf("completions = %d, want 1", got)
	}

	var (
		outcome   string
		endedAt   *time.Time
		model     string
		tokensIn  int64
		tokensOut int64
		cost      string
		flags     string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT outcome, ended_at, model, tokens_in, tokens_out, cost_usd::text, injection_flags::text
		   FROM agent_runs WHERE id = $1::uuid`, runID).Scan(
		&outcome, &endedAt, &model, &tokensIn, &tokensOut, &cost, &flags); err != nil {
		t.Fatalf("read agent run: %v", err)
	}
	if outcome != string(agent.OutcomeEscalated) || endedAt == nil {
		t.Fatalf("run outcome/ended = %s/%v", outcome, endedAt)
	}
	if model != "test-model-2026" || tokensIn != 10 || tokensOut != 5 || cost != "0.020000" {
		t.Fatalf("run row = model %s tokens %d/%d cost %s", model, tokensIn, tokensOut, cost)
	}
	if flags != "[]" {
		t.Fatalf("injection_flags = %s, want []", flags)
	}

	var (
		callSeq     int
		requestHash string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT seq, request_hash FROM llm_calls WHERE agent_run_id = $1::uuid`, runID).Scan(
		&callSeq, &requestHash); err != nil {
		t.Fatalf("read llm call: %v", err)
	}
	requests := scripted.Requests()
	canonical, err := llm.CanonicalRequestHash(cfg.LLM.Provider, cfg.LLM.BaseURL, requests[0])
	if err != nil {
		t.Fatalf("CanonicalRequestHash: %v", err)
	}
	if callSeq != 1 || requestHash != canonical {
		t.Fatalf("llm call seq/hash = %d/%s, want 1/%s", callSeq, requestHash, canonical)
	}

	var toolCount int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM tool_calls WHERE agent_run_id = $1::uuid`, runID).Scan(&toolCount); err != nil {
		t.Fatalf("count tool calls: %v", err)
	}
	if toolCount != 2 {
		t.Fatalf("tool_calls = %d, want seed get_case + escalate", toolCount)
	}

	var (
		status  string
		version int
		reason  *string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT status, version, escalation_reason FROM cases WHERE id = $1::uuid`, caseID).Scan(
		&status, &version, &reason); err != nil {
		t.Fatalf("read case: %v", err)
	}
	if status != "escalated" || version != 1 || reason == nil || *reason != "ambiguous_evidence" {
		t.Fatalf("case = %s v%d reason %v", status, version, reason)
	}

	// FIN-24/FIN-30/FIN-33: the loop-owned transition is audited with the
	// agent actor derived from the configured model.
	var (
		actorType string
		actorID   string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE entity_type = 'case' AND entity_id = $1
		  ORDER BY seq DESC LIMIT 1`, caseID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if actorType != "agent" || actorID != "test-model" {
		t.Fatalf("audit actor = %s:%s, want agent:test-model", actorType, actorID)
	}
}
