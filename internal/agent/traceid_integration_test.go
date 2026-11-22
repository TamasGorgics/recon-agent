//go:build integration

package agent_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestTracedAgentRunPersistsTraceID proves the traced scripted path writes one
// valid trace ID on agent_runs and proposals, while a non-traced run of a
// different case leaves both NULL.
func TestTracedAgentRunPersistsTraceID(t *testing.T) {
	ctx := context.Background()
	d := newAgentTracingDB(t)
	app := openGateRolePool(t, d, db.RoleApp)
	agentRO := openGateRolePool(t, d, db.RoleAgentRO)
	cfg := gateConfig()
	replay := newAgentReplayCassette(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scenarios := m2.Scenarios()
	if len(scenarios) < 3 {
		t.Fatalf("M2 scenarios = %d, want at least 3", len(scenarios))
	}

	exporter := tracetest.NewInMemoryExporter()
	provider, err := telemetry.New(ctx, telemetry.Config{ServiceName: "agent-test"},
		telemetry.WithExporter(exporter), telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	traced, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        scenarios[0].CaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: "v1",
	}, agentrun.Deps{
		Config:    cfg,
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Clock:     clock.Real{},
		Logger:    logger,
		Client:    replay,
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "cli:trace:traced",
		Telemetry: provider,
	})
	if err != nil {
		t.Fatalf("traced run: %v", err)
	}
	if traced.ProposalID == "" {
		t.Fatal("traced run produced no proposal")
	}

	runTraceID := readAgentRunTraceID(t, app, traced.RunID)
	if !telemetry.ValidTraceID(runTraceID) {
		t.Fatalf("agent_runs.trace_id = %q, want 32 lowercase hex", runTraceID)
	}
	proposalTraceID := readProposalTraceID(t, app, traced.ProposalID)
	if proposalTraceID != runTraceID {
		t.Fatalf("proposals.trace_id = %q, want run trace %q", proposalTraceID, runTraceID)
	}
	spans := exporter.GetSpans()
	rootTraceID := ""
	for _, span := range spans {
		if span.Name == "case.run" {
			rootTraceID = span.SpanContext.TraceID().String()
			break
		}
	}
	if rootTraceID != runTraceID {
		t.Fatalf("case.run trace = %q, want agent_runs.trace_id %q", rootTraceID, runTraceID)
	}

	nonTraced, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        scenarios[1].CaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: "v1",
	}, agentrun.Deps{
		Config:    cfg,
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Clock:     clock.Real{},
		Logger:    logger,
		Client:    replay,
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "cli:trace:plain",
	})
	if err != nil {
		t.Fatalf("non-traced run: %v", err)
	}
	if nonTraced.ProposalID == "" {
		t.Fatal("non-traced run produced no proposal")
	}
	if got := readAgentRunTraceID(t, app, nonTraced.RunID); got != "" {
		t.Fatalf("non-traced agent_runs.trace_id = %q, want NULL", got)
	}
	if got := readProposalTraceID(t, app, nonTraced.ProposalID); got != "" {
		t.Fatalf("non-traced proposals.trace_id = %q, want NULL", got)
	}

	// A legacy WithTraceID value models the API middleware's correlation ID
	// derived from an unauthenticated inbound traceparent. With no active span
	// and no provider, neither the run row nor the proposal row may persist it.
	forgedCtx := telemetry.WithTraceID(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	forged, err := agentrun.Run(forgedCtx, agentrun.Options{
		CaseID:        scenarios[2].CaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: "v1",
	}, agentrun.Deps{
		Config:    cfg,
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Clock:     clock.Real{},
		Logger:    logger,
		Client:    replay,
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "cli:trace:forged",
	})
	if err != nil {
		t.Fatalf("legacy-only run: %v", err)
	}
	if forged.ProposalID == "" {
		t.Fatal("legacy-only run produced no proposal")
	}
	if got := readAgentRunTraceID(t, app, forged.RunID); got != "" {
		t.Fatalf("legacy-only agent_runs.trace_id = %q, want NULL", got)
	}
	if got := readProposalTraceID(t, app, forged.ProposalID); got != "" {
		t.Fatalf("legacy-only proposals.trace_id = %q, want NULL", got)
	}
}

func newAgentTracingDB(t *testing.T) *testdb.DB {
	t.Helper()
	d := testdb.New(t)
	if err := m2.Load(context.Background(), d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	return d
}

func newAgentReplayCassette(t *testing.T, cfg config.Config) llm.Client {
	t.Helper()
	replay, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      filepath.Join(gateRepoRoot(t), "eval", "cassettes"),
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
	})
	if err != nil {
		t.Fatalf("build replay cassette: %v", err)
	}
	return replay
}

func readAgentRunTraceID(t *testing.T, app *pgxpool.Pool, runID string) string {
	t.Helper()
	var traceID string
	if err := app.QueryRow(context.Background(),
		`SELECT COALESCE(trace_id, '') FROM agent_runs WHERE id = $1::uuid`,
		runID).Scan(&traceID); err != nil {
		t.Fatalf("read agent run trace_id: %v", err)
	}
	return traceID
}

func readProposalTraceID(t *testing.T, app *pgxpool.Pool, proposalID string) string {
	t.Helper()
	var traceID string
	if err := app.QueryRow(context.Background(),
		`SELECT COALESCE(trace_id, '') FROM proposals WHERE id = $1::uuid`,
		proposalID).Scan(&traceID); err != nil {
		t.Fatalf("read proposal trace_id: %v", err)
	}
	return traceID
}
