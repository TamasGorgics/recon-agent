//go:build integration

package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

// TestMilestone2Gate is the M2 exit gate: three recorded easy scenarios must
// produce schema-valid proposals with the locked types, and a simulated
// provider outage must leave the case queued with no attempt.
func TestMilestone2Gate(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if err := m2.Load(ctx, d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	app := openGateRolePool(t, d, db.RoleApp)
	agentRO := openGateRolePool(t, d, db.RoleAgentRO)

	cfg := gateConfig()
	cassetteDir := filepath.Join(gateRepoRoot(t), "eval", "cassettes")
	replay, err := llm.NewCassette(nil, llm.CassetteOptions{
		Mode:     llm.CassetteReplay,
		Dir:      cassetteDir,
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
	})
	if err != nil {
		t.Fatalf("build replay cassette: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	timeSource := clock.Real{}
	denied := slog.New(slog.NewTextHandler(io.Discard, nil))

	reports := make(map[string]agentrun.Report, len(m2.Scenarios()))
	for _, scenario := range m2.Scenarios() {
		report, runErr := agentrun.Run(ctx, agentrun.Options{
			CaseID:        scenario.CaseID,
			Mode:          agentrun.ModeScripted,
			PromptVersion: "v1",
		}, agentrun.Deps{
			Config:    cfg,
			App:       app,
			Agent:     agentRO,
			AgentExec: agentRO,
			Clock:     timeSource,
			Logger:    logger,
			Client:    replay,
			NewRunID:  agent.DeterministicRunID,
			WorkerID:  "cli:gate:" + scenario.Mode,
		})
		if runErr != nil {
			if errors.Is(runErr, llm.ErrCassetteMiss) {
				t.Fatalf("%s: cassette miss: re-record eval/cassettes after any prompt/tool/fixture change: %v",
					scenario.Mode, runErr)
			}
			t.Fatalf("%s: run: %v", scenario.Mode, runErr)
		}
		if report.Outcome != string(agent.OutcomeProposal) {
			t.Fatalf("%s: outcome = %s, want proposal", scenario.Mode, report.Outcome)
		}
		if report.ProposalType != scenario.ExpectedType {
			t.Fatalf("%s: proposal type = %s, want %s", scenario.Mode, report.ProposalType, scenario.ExpectedType)
		}
		reports[scenario.Mode] = report
	}

	for _, scenario := range m2.Scenarios() {
		report := reports[scenario.Mode]
		assertGateProposal(t, ctx, app, scenario, report)
	}

	cassetteCount := countCassettes(t, cassetteDir)
	t.Logf("M2-11 gate: fixture=eval/fixtures/m2@v1 cassettes=%d prompt=v1 model=%s", cassetteCount, cfg.Agent.Model)
	for _, scenario := range m2.Scenarios() {
		report := reports[scenario.Mode]
		t.Logf("M2-11 gate: scenario=%s outcome=%s proposal=%s steps=%d tool_calls=%d tokens=%d/%d cost=%s",
			scenario.Mode, report.Outcome, report.ProposalType, report.Steps,
			report.ToolCalls, report.TokensIn, report.TokensOut, report.CostUSD.String())
	}

	// Simulated total provider outage through the production worker policy.
	outageCaseID := insertOutageCase(ctx, t, d)
	outageRunner := buildOutageRunner(t, app, agentRO, cfg, logger)
	outageWorker := buildOutageWorker(t, app, outageRunner, denied)
	if err := outageWorker.ProcessOnce(ctx); err != nil {
		t.Fatalf("outage ProcessOnce: %v", err)
	}
	var (
		status   string
		attempts int
		leasedBy *string
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT status, attempts, leased_by FROM cases WHERE id = $1::uuid`, outageCaseID).
		Scan(&status, &attempts, &leasedBy); err != nil {
		t.Fatalf("read outage case: %v", err)
	}
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("outage case = status %s attempts %d leased_by %v, want open/0/nil",
			status, attempts, leasedBy)
	}
	var (
		outageOutcome string
		llmCalls      int
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT COALESCE(outcome, ''), (SELECT count(*) FROM llm_calls l WHERE l.agent_run_id = r.id)
		   FROM agent_runs r WHERE r.case_id = $1::uuid
		  ORDER BY r.started_at DESC LIMIT 1`, outageCaseID).Scan(&outageOutcome, &llmCalls); err != nil {
		t.Fatalf("read outage run: %v", err)
	}
	if outageOutcome != string(agent.OutcomeProviderError) {
		t.Fatalf("outage run outcome = %q, want provider_error", outageOutcome)
	}
	if llmCalls != 0 {
		t.Fatalf("outage run llm_calls = %d, want 0", llmCalls)
	}
	t.Logf("M2-11 gate: outage outcome=%s case_status=%s attempts=%d llm_calls=%d",
		outageOutcome, status, attempts, llmCalls)

	// covers: FIN-24
	// The outage release is owner-guarded and must not increment attempts.
}

func assertGateProposal(t *testing.T, ctx context.Context, app *pgxpool.Pool, scenario m2.Scenario, report agentrun.Report) {
	t.Helper()
	var (
		proposalType string
		status       string
		riskTier     *string
		evidenceRaw  []byte
		stateHash    string
		confidence   string
		runOutcome   string
		promptVer    string
		tokens       int64
		cost         string
		runID        string
	)
	if err := app.QueryRow(ctx,
		`SELECT p.type, p.status, p.risk_tier, p.evidence, p.state_hash,
		        p.confidence::text, r.outcome, r.prompt_version,
		        r.tokens_in + r.tokens_out, r.cost_usd::text, r.id::text
		   FROM proposals p
		   JOIN agent_runs r ON r.id = p.agent_run_id
		  WHERE p.case_id = $1::uuid
		  ORDER BY p.created_at DESC LIMIT 1`, scenario.CaseID).Scan(
		&proposalType, &status, &riskTier, &evidenceRaw, &stateHash,
		&confidence, &runOutcome, &promptVer, &tokens, &cost, &runID); err != nil {
		t.Fatalf("%s: read proposal: %v", scenario.Mode, err)
	}
	if proposalType != scenario.ExpectedType {
		t.Fatalf("%s: proposal type = %s, want %s", scenario.Mode, proposalType, scenario.ExpectedType)
	}
	if status != "proposed" {
		t.Fatalf("%s: proposal status = %s, want proposed", scenario.Mode, status)
	}
	if riskTier != nil {
		t.Fatalf("%s: risk_tier = %q, want NULL", scenario.Mode, *riskTier)
	}
	var evidence []string
	if err := json.Unmarshal(evidenceRaw, &evidence); err != nil {
		t.Fatalf("%s: decode evidence: %v", scenario.Mode, err)
	}
	if len(evidence) == 0 {
		t.Fatalf("%s: evidence is empty", scenario.Mode)
	}
	for _, ref := range evidence {
		if err := tools.ValidateRef(ref); err != nil {
			t.Fatalf("%s: evidence ref %q: %v", scenario.Mode, ref, err)
		}
	}
	if strings.TrimSpace(stateHash) == "" {
		t.Fatalf("%s: state_hash is empty", scenario.Mode)
	}
	conf, err := decimal.NewFromString(confidence)
	if err != nil || conf.IsNegative() || conf.GreaterThan(decimal.NewFromInt(1)) {
		t.Fatalf("%s: confidence = %q, want [0,1]", scenario.Mode, confidence)
	}
	if runOutcome != string(agent.OutcomeProposal) {
		t.Fatalf("%s: agent_runs.outcome = %q, want proposal", scenario.Mode, runOutcome)
	}
	if promptVer != "v1" {
		t.Fatalf("%s: prompt_version = %q, want v1", scenario.Mode, promptVer)
	}
	if tokens <= 0 {
		t.Fatalf("%s: run tokens = %d, want > 0", scenario.Mode, tokens)
	}
	if _, err := decimal.NewFromString(cost); err != nil {
		t.Fatalf("%s: cost_usd = %q is not a decimal", scenario.Mode, cost)
	}

	var (
		llmCalls     int
		hashesValid  bool
		proposalRuns int
	)
	if err := app.QueryRow(ctx,
		`SELECT count(*), COALESCE(bool_and(request_hash ~ '^[0-9a-f]{64}$'), true)
		   FROM llm_calls WHERE agent_run_id = $1::uuid`, runID).
		Scan(&llmCalls, &hashesValid); err != nil {
		t.Fatalf("%s: read llm_calls: %v", scenario.Mode, err)
	}
	if llmCalls != report.Steps {
		t.Fatalf("%s: llm_calls = %d, steps = %d", scenario.Mode, llmCalls, report.Steps)
	}
	if !hashesValid {
		t.Fatalf("%s: a request_hash is not 64 lowercase hex", scenario.Mode)
	}
	if err := app.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE id = $1::uuid AND outcome = 'proposal'`, runID).
		Scan(&proposalRuns); err != nil {
		t.Fatalf("%s: read proposal run: %v", scenario.Mode, err)
	}
	if proposalRuns != 1 {
		t.Fatalf("%s: proposal run not found", scenario.Mode)
	}

	var caseStatus string
	if err := app.QueryRow(ctx,
		`SELECT status FROM cases WHERE id = $1::uuid`, scenario.CaseID).Scan(&caseStatus); err != nil {
		t.Fatalf("%s: read case: %v", scenario.Mode, err)
	}
	if caseStatus != "proposal_pending" {
		t.Fatalf("%s: case status = %s, want proposal_pending", scenario.Mode, caseStatus)
	}
}

func gateConfig() config.Config {
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

func openGateRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.OpenRole(context.Background(), role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s role pool: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool.Pool
}

func insertOutageCase(ctx context.Context, t *testing.T, d *testdb.DB) string {
	t.Helper()
	const outageCaseID = "10000000-0000-4000-8000-000000000004"
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO cases
		     (id, priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES ($1::uuid, 0, 1000, 'USD', 'open', 0, 0, $2)`,
		outageCaseID, m2.FixedInstant); err != nil {
		t.Fatalf("insert outage case: %v", err)
	}
	return outageCaseID
}

func buildOutageRunner(t *testing.T, app, agentRO *pgxpool.Pool, cfg config.Config, logger *slog.Logger) *agent.Runner {
	t.Helper()
	scripted := llm.NewScripted(llm.ScriptedStep{Err: &llm.ProviderError{
		Provider:  llm.ProviderOpenCodeGo,
		Kind:      llm.KindUnavailable,
		Retryable: true,
		Message:   "simulated provider outage",
	}})
	runner, err := agent.NewProductionRunner(agent.SetupDeps{
		App:       app,
		Agent:     agentRO,
		AgentExec: agentRO,
		Client:    scripted,
		Clock:     clock.Real{},
		Config:    cfg,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("build outage runner: %v", err)
	}
	return runner
}

func buildOutageWorker(t *testing.T, app *pgxpool.Pool, runner *agent.Runner, logger *slog.Logger) *worker.Worker {
	t.Helper()
	actor, err := audit.SystemActor("gate")
	if err != nil {
		t.Fatalf("gate actor: %v", err)
	}
	svc, err := cases.NewService(app, clock.Real{}, actor)
	if err != nil {
		t.Fatalf("gate case service: %v", err)
	}
	w, err := worker.New(worker.Options{
		ID:      "worker:gate:1",
		Config:  config.DefaultWorkerConfig(),
		Cases:   svc,
		Runner:  runner,
		Switch:  agent.DBSwitch{Queryer: app},
		Spend:   worker.DBSpend{Queryer: app},
		Clock:   clock.Real{},
		Sleeper: worker.TimerSleeper{},
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("build outage worker: %v", err)
	}
	return w
}

func countCassettes(t *testing.T, dir string) int {
	t.Helper()
	var count int
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cassettes: %v", err)
	}
	return count
}

func gateRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
