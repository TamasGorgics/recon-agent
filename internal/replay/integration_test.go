//go:build integration

package replay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func TestMain(m *testing.M) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "replay: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(context.Background(), testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "replay: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestExactReplayNoDBAfterLoad records a real run through the production loop,
// loads it once in a read-only transaction, closes every pool, proves a
// post-load query fails, and then replays the in-memory recording. The scripted
// client proves no network path is reachable from ExactReplay.
func TestExactReplayNoDBAfterLoad(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if err := m2.Load(ctx, d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	scenario := m2.Scenarios()[0]
	cfg := replayTestConfig()
	app := openRolePool(t, d, db.RoleApp)
	agentRO := openRolePool(t, d, db.RoleAgentRO)

	scripted := llm.NewScripted(
		llm.ScriptedStep{Response: scriptedGetCaseResponse(t, scenario.CaseID, cfg)},
		llm.ScriptedStep{Response: scriptedRequestInfoResponse(t, scenario.CaseID, cfg)},
	)
	recorded, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        scenario.CaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: cfg.Agent.PromptVersion,
	}, agentrun.Deps{
		Config:    cfg,
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Clock:     clock.Real{},
		Logger:    discardLogger(),
		Client:    scripted,
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "test:replay",
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if recorded.Outcome != string(agent.OutcomeProposal) {
		t.Fatalf("outcome = %s, want proposal", recorded.Outcome)
	}
	if scripted.Calls() != 2 {
		t.Fatalf("scripted calls = %d, want 2", scripted.Calls())
	}

	rec := loadRecordingReadOnly(t, ctx, app, recorded.RunID)
	if rec.Run.Outcome != string(agent.OutcomeProposal) || rec.Proposal == nil {
		t.Fatalf("loaded run outcome = %q, proposal = %v", rec.Run.Outcome, rec.Proposal)
	}
	if len(rec.LLMCalls) != 2 || len(rec.ToolCalls) != 3 {
		t.Fatalf("loaded llm calls = %d, tool calls = %d, want 2/3", len(rec.LLMCalls), len(rec.ToolCalls))
	}
	for _, call := range rec.ToolCalls {
		if call.ResultCanonicalHash == nil {
			t.Fatalf("tool_calls.seq %d has a NULL canonical hash for a newly recorded run", call.Seq)
		}
	}

	// The load phase is over: close every pool and prove the database is
	// unreachable before ExactReplay runs.
	app.Close()
	agentRO.Close()
	var one int
	if err := app.QueryRow(ctx, `SELECT 1`).Scan(&one); err == nil {
		t.Fatal("closed app pool still answered a query")
	}
	if err := agentRO.QueryRow(ctx, `SELECT 1`).Scan(&one); err == nil {
		t.Fatal("closed agent pool still answered a query")
	}

	report, err := replay.ExactReplay(rec, replay.Config{
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Agent:    cfg.Agent,
	})
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false: divergences=%+v diagnostics=%+v", report.Divergences, report.Diagnostics)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
	if report.Outcome != string(agent.OutcomeProposal) {
		t.Fatalf("report outcome = %q", report.Outcome)
	}
	if scripted.Calls() != 2 {
		t.Fatalf("replay called the LLM client: calls = %d, want 2", scripted.Calls())
	}
	if len(report.Clock) == 0 {
		t.Fatal("report has no fake-clock events")
	}
	for _, event := range report.Clock {
		if event.At.IsZero() {
			t.Fatalf("fake-clock event has a zero timestamp: %+v", event)
		}
	}
}

// TestExactReplayMalformedProposalRepairFromRecording records the real
// production flow "malformed proposal arguments -> invalid_arguments result ->
// repair message -> valid proposal" through the production loop and recorder,
// then loads the run and requires exact replay to be identical. It proves the
// repair path is reachable with real rows, that the recorder stores the
// normalized empty-argument body, and that replay does not report a false
// step_count_mismatch or a false tool-args mismatch.
func TestExactReplayMalformedProposalRepairFromRecording(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if err := m2.Load(ctx, d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	scenario := m2.Scenarios()[0]
	cfg := replayTestConfig()
	app := openRolePool(t, d, db.RoleApp)
	agentRO := openRolePool(t, d, db.RoleAgentRO)

	malformed := scriptedRequestInfoResponse(t, scenario.CaseID, cfg)
	malformed.Message.ToolCalls[0].Arguments = nil
	scripted := llm.NewScripted(
		llm.ScriptedStep{Response: scriptedGetCaseResponse(t, scenario.CaseID, cfg)},
		llm.ScriptedStep{Response: malformed},
		llm.ScriptedStep{Response: scriptedRequestInfoResponse(t, scenario.CaseID, cfg)},
	)
	recorded, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        scenario.CaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: cfg.Agent.PromptVersion,
	}, agentrun.Deps{
		Config:    cfg,
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Clock:     clock.Real{},
		Logger:    discardLogger(),
		Client:    scripted,
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "test:replay-repair",
	})
	if err != nil {
		t.Fatalf("record repaired run: %v", err)
	}
	if recorded.Outcome != string(agent.OutcomeProposal) {
		t.Fatalf("outcome = %s, want proposal", recorded.Outcome)
	}
	if scripted.Calls() != 3 {
		t.Fatalf("scripted calls = %d, want 3 (get_case, malformed, valid proposal)", scripted.Calls())
	}

	rec := loadRecordingReadOnly(t, ctx, app, recorded.RunID)
	if len(rec.LLMCalls) != 3 || len(rec.ToolCalls) != 4 {
		t.Fatalf("loaded llm calls = %d, tool calls = %d, want 3/4", len(rec.LLMCalls), len(rec.ToolCalls))
	}
	// seq 3 is the malformed proposal attempt: the runner normalized the empty
	// arguments to {} and recorded invalid_arguments; seq 4 is the valid retry.
	if got := string(rec.ToolCalls[2].Args); got != "{}" {
		t.Fatalf("malformed call args = %q, want the recorded normalized empty object", got)
	}
	if rec.ToolCalls[2].Error == nil || *rec.ToolCalls[2].Error != "invalid_arguments" {
		t.Fatalf("malformed call error = %v, want invalid_arguments", rec.ToolCalls[2].Error)
	}
	if callErr := rec.ToolCalls[3].Error; callErr != nil {
		t.Fatalf("valid proposal call error = %q, want none", *callErr)
	}

	app.Close()
	agentRO.Close()
	report, err := replay.ExactReplay(rec, replay.Config{
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Agent:    cfg.Agent,
	})
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("repaired run is not identical: divergences=%+v diagnostics=%+v", report.Divergences, report.Diagnostics)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
	if report.StepsTotal != 3 {
		t.Fatalf("steps_total = %d, want 3", report.StepsTotal)
	}
}

func loadRecordingReadOnly(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID string) replay.Recording {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	rec, err := replay.LoadRecording(ctx, tx, runID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("LoadRecording(): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit read-only load: %v", err)
	}
	return rec
}

func scriptedGetCaseResponse(t *testing.T, caseID string, cfg config.Config) llm.Response {
	t.Helper()
	args, err := json.Marshal(map[string]any{"case_id": caseID})
	if err != nil {
		t.Fatalf("marshal get_case args: %v", err)
	}
	return llm.Response{
		Provider:       cfg.LLM.Provider,
		RequestedModel: llm.APIID(cfg.Agent.Model),
		ActualModel:    llm.APIID(cfg.Agent.Model),
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "get_case",
				Arguments: args,
			}},
		},
		StopReason: llm.StopToolCalls,
		Usage:      llm.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
		Latency:    2 * time.Millisecond,
	}
}

func scriptedRequestInfoResponse(t *testing.T, caseID string, cfg config.Config) llm.Response {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"case_id":    caseID,
		"rationale":  "Need the settlement reference before deciding.",
		"evidence":   []string{"case:" + caseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
	if err != nil {
		t.Fatalf("marshal proposal args: %v", err)
	}
	return llm.Response{
		Provider:       cfg.LLM.Provider,
		RequestedModel: llm.APIID(cfg.Agent.Model),
		ActualModel:    llm.APIID(cfg.Agent.Model),
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "request_info",
				Arguments: args,
			}},
		},
		StopReason: llm.StopToolCalls,
		Usage:      llm.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
		Latency:    3 * time.Millisecond,
	}
}

func replayTestConfig() config.Config {
	cfg := config.Config{
		LLM:    config.DefaultLLMConfig(),
		Agent:  config.DefaultAgentConfig(),
		Worker: config.DefaultWorkerConfig(),
		Ledger: config.DefaultLedgerConfig(),
		Ingest: m2.IngestConfig(),
	}
	cfg.Agent.Model = config.DefaultLLMModel
	cfg.Agent.PromptVersion = "v1"
	return cfg
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s role pool: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool.Pool
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
