//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

type integrationFixture struct {
	db       *testdb.DB
	app      *pgxpool.Pool
	agentRO  *pgxpool.Pool
	cases    *cases.Service
	clock    *clock.Fake
	workerID string
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 16})
	app := openRolePool(t, d, db.RoleApp)
	agentRO := openRolePool(t, d, db.RoleAgentRO)
	fakeClock := clock.NewFake(time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC))
	actor, err := audit.SystemActor("worker")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	svc, err := cases.NewService(app, fakeClock, actor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}
	return &integrationFixture{
		db: d, app: app, agentRO: agentRO, cases: svc,
		clock: fakeClock, workerID: "worker:test:4242:deadbeef",
	}
}

func openRolePool(t *testing.T, d *testdb.DB, role string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := db.OpenRole(ctx, role, d.DSNFor(role))
	if err != nil {
		t.Fatalf("open %s pool: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool.Pool
}

func (f *integrationFixture) insertCase(t *testing.T, status string, attempts int) string {
	t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(context.Background(),
		`INSERT INTO cases
		     (priority, value_minor, currency, created_at, version, attempts, status)
		 VALUES (2, 1000, 'USD', $1, 0, $2, $3)
		 RETURNING id::text`, f.clock.Now(), attempts, status).Scan(&id); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	return id
}

func (f *integrationFixture) insertSpendRun(t *testing.T, caseID, cost string) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`INSERT INTO agent_runs
		     (case_id, model, prompt_version, started_at, ended_at,
		      tokens_in, tokens_out, cost_usd, outcome)
		 VALUES ($1::uuid, 'test-model', 'v1', $2, $2, 1, 1, $3::numeric, 'provider_error')`,
		caseID, f.clock.Now(), cost); err != nil {
		t.Fatalf("insert spend run: %v", err)
	}
}

func (f *integrationFixture) setSwitch(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE system_switches SET enabled = $1 WHERE name = 'agent_enabled'`, enabled); err != nil {
		t.Fatalf("set agent_enabled=%v: %v", enabled, err)
	}
}

func (f *integrationFixture) integrationConfig() config.Config {
	return config.Config{
		LLM:    config.DefaultLLMConfig(),
		Agent:  config.DefaultAgentConfig(),
		Worker: config.DefaultWorkerConfig(),
		Ledger: config.DefaultLedgerConfig(),
	}
}

func (f *integrationFixture) newRunner(t *testing.T, client llm.Client, mutate func(*config.Config)) *agent.Runner {
	t.Helper()
	cfg := f.integrationConfig()
	cfg.Agent.Model = "test-model"
	if mutate != nil {
		mutate(&cfg)
	}
	runner, err := agent.NewProductionRunner(agent.SetupDeps{
		App:       f.app,
		Agent:     f.agentRO,
		AgentExec: f.agentRO,
		Client:    client,
		Clock:     f.clock,
		Config:    cfg,
		Logger:    quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewProductionRunner: %v", err)
	}
	return runner
}

func (f *integrationFixture) newWorker(t *testing.T, id string, runner worker.Runner, mutate func(*config.WorkerConfig)) *worker.Worker {
	t.Helper()
	cfg := config.DefaultWorkerConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := worker.New(worker.Options{
		ID:      id,
		Config:  cfg,
		Cases:   f.cases,
		Runner:  runner,
		Switch:  agent.DBSwitch{Queryer: f.app},
		Spend:   worker.DBSpend{Queryer: f.app},
		Clock:   f.clock,
		Sleeper: worker.TimerSleeper{},
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return w
}

func (f *integrationFixture) caseRow(t *testing.T, caseID string) (status string, attempts int, leasedBy *string, reason *string) {
	t.Helper()
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT status, attempts, leased_by, escalation_reason
		   FROM cases WHERE id = $1::uuid`, caseID).Scan(&status, &attempts, &leasedBy, &reason); err != nil {
		t.Fatalf("read case: %v", err)
	}
	return status, attempts, leasedBy, reason
}

func (f *integrationFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func seenRunnerIDs(r *fakeRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (f *integrationFixture) providerDownStep() llm.ScriptedStep {
	return llm.ScriptedStep{Err: &llm.ProviderError{
		Provider: "test", Kind: llm.KindUnavailable, Retryable: true, Message: "provider down",
	}}
}

func (f *integrationFixture) completionStep(caseID string) llm.ScriptedStep {
	return llm.ScriptedStep{Response: llm.Response{
		Provider:    llm.ProviderOpenCodeGo,
		ActualModel: "test-model-2026",
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call_1", Name: "get_case",
			Arguments: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, caseID)),
		}}},
		StopReason: llm.StopToolCalls,
		Usage: llm.Usage{
			PromptTokens: 10, CompletionTokens: 0,
			CostUSD: decimal.RequireFromString("0.01"),
		},
	}}
}

// TestWorkerTotalProviderOutageLeavesCasesQueued is the M2 exit-gate outage
// scenario at the worker level: every run is provider_error, the case stays
// open, and attempts never move.
func TestWorkerTotalProviderOutageLeavesCasesQueued(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 0)

	scripted := llm.NewScripted(f.providerDownStep(), f.providerDownStep(), f.providerDownStep())
	runner := f.newRunner(t, scripted, nil)
	w := f.newWorker(t, f.workerID, runner, nil)

	for i := 0; i < 3; i++ {
		if err := w.ProcessOnce(ctx); err != nil {
			t.Fatalf("ProcessOnce %d: %v", i, err)
		}
	}
	if got := scripted.Calls(); got != 3 {
		t.Fatalf("completions = %d, want 3", got)
	}
	status, attempts, leasedBy, _ := f.caseRow(t, caseID)
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want open/0/nil", status, attempts, leasedBy)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM agent_runs WHERE case_id = $1::uuid AND outcome = 'provider_error' AND ended_at IS NOT NULL`,
		caseID), 3; got != want {
		t.Fatalf("provider_error runs = %d, want %d", got, want)
	}
}

// TestWorkerPoisonCaseEscalatesAfterThreeAttempts proves a poison case gets
// exactly max_attempts model calls and is escalated agent_failed on the third
// counted failure; there is no fourth lease.
func TestWorkerPoisonCaseEscalatesAfterThreeAttempts(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 0)

	steps := []llm.ScriptedStep{
		f.completionStep(caseID), f.completionStep(caseID), f.completionStep(caseID),
	}
	scripted := llm.NewScripted(steps...)
	// One completion per run pushes the token budget over the cap, producing
	// budget_exhausted without executing a tool.
	runner := f.newRunner(t, scripted, func(cfg *config.Config) { cfg.Agent.MaxTotalTokens = 1 })
	w := f.newWorker(t, f.workerID, runner, nil)

	for i := 0; i < 3; i++ {
		if err := w.ProcessOnce(ctx); err != nil {
			t.Fatalf("ProcessOnce %d: %v", i, err)
		}
	}
	if got := scripted.Calls(); got != 3 {
		t.Fatalf("completions = %d, want exactly 3", got)
	}
	if err := w.ProcessOnce(ctx); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("fourth ProcessOnce = %v, want ErrNoCase", err)
	}
	if got := scripted.Calls(); got != 3 {
		t.Fatalf("completions after fourth poll = %d, want 3", got)
	}

	status, attempts, leasedBy, reason := f.caseRow(t, caseID)
	if status != "escalated" || attempts != 3 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want escalated/3/nil", status, attempts, leasedBy)
	}
	if reason == nil || *reason != "agent_failed" {
		t.Fatalf("escalation reason = %v, want agent_failed", reason)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM agent_runs WHERE case_id = $1::uuid AND outcome = 'budget_exhausted'`,
		caseID), 3; got != want {
		t.Fatalf("budget_exhausted runs = %d, want %d", got, want)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'case' AND entity_id = $1 AND action = $2`,
		caseID, cases.ActionCaseAttempt), 3; got != want {
		t.Fatalf("case.attempt audit rows = %d, want %d", got, want)
	}
	var actorType, actorID string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE entity_type = 'case' AND entity_id = $1 AND action = $2
		  ORDER BY seq DESC LIMIT 1`, caseID, cases.ActionCaseAttempt).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read attempt audit: %v", err)
	}
	if actorType != "system" || actorID != "worker" {
		t.Fatalf("attempt audit actor = %s:%s, want system:worker", actorType, actorID)
	}
}

// TestWorkerSwitchOffMidRunReleasesWithoutAttempt flips agent_enabled off after
// the first completion; the loop stops before its next model call, the run is
// finalized escalated, and the case is released with no attempt.
func TestWorkerSwitchOffMidRunReleasesWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 0)

	step := f.completionStep(caseID)
	step.Check = func(llm.Request) error {
		_, err := f.db.Pool.Exec(context.Background(),
			`UPDATE system_switches SET enabled = false WHERE name = 'agent_enabled'`)
		return err
	}
	scripted := llm.NewScripted(step)
	runner := f.newRunner(t, scripted, nil)
	w := f.newWorker(t, f.workerID, runner, nil)

	if err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if got := scripted.Calls(); got != 1 {
		t.Fatalf("completions = %d, want 1 (switch must stop before the next call)", got)
	}
	status, attempts, leasedBy, _ := f.caseRow(t, caseID)
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want open/0/nil", status, attempts, leasedBy)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM agent_runs WHERE case_id = $1::uuid AND outcome = 'escalated' AND ended_at IS NOT NULL`,
		caseID), 1; got != want {
		t.Fatalf("escalated runs = %d, want %d", got, want)
	}

	// The switch is now off: the worker must not lease.
	if err := w.ProcessOnce(ctx); !errors.Is(err, worker.ErrAgentDisabled) {
		t.Fatalf("ProcessOnce with switch off = %v, want ErrAgentDisabled", err)
	}
}

// TestWorkerDailyBudgetStopsNewLeases seeds the UTC-day spend at the cap and
// proves the open case is left untouched.
func TestWorkerDailyBudgetStopsNewLeases(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	historyCase := f.insertCase(t, "escalated", 3)
	f.insertSpendRun(t, historyCase, "5.00")
	target := f.insertCase(t, "open", 0)

	runner := &fakeRunner{}
	w := f.newWorker(t, f.workerID, runner, nil)
	if err := w.ProcessOnce(ctx); !errors.Is(err, worker.ErrDailyBudgetReached) {
		t.Fatalf("ProcessOnce = %v, want ErrDailyBudgetReached", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runs = %d, want 0", runner.callCount())
	}
	status, attempts, leasedBy, _ := f.caseRow(t, target)
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("target case = %s attempts=%d leased_by=%v, want open/0/nil", status, attempts, leasedBy)
	}
}

// TestWorkerConcurrentWorkersNeverLeaseSameCase races eight workers over eight
// cases through the real SKIP LOCKED lease; every case is leased exactly once.
func TestWorkerConcurrentWorkersNeverLeaseSameCase(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	const n = 8
	for i := 0; i < n; i++ {
		f.insertCase(t, "open", 0)
	}

	started := make(chan string, n)
	gate := make(chan struct{})
	runner := &fakeRunner{fn: func(_ context.Context, caseID string) (agent.Result, error) {
		started <- caseID
		<-gate
		return agent.Result{RunID: "run-" + caseID, Outcome: agent.OutcomeBudgetExhausted}, nil
	}}
	workers := make([]*worker.Worker, n)
	for i := 0; i < n; i++ {
		workers[i] = f.newWorker(t, fmt.Sprintf("worker:test:%d:%08x", i, i), runner, nil)
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- workers[i].ProcessOnce(ctx)
		}(i)
	}
	// Every worker must hold a distinct lease before any case is released, so
	// a slow worker cannot re-lease an already-processed case.
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for worker %d", i)
		}
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ProcessOnce: %v", err)
		}
	}

	seen := make(map[string]struct{}, n)
	for _, id := range seenRunnerIDs(runner) {
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("distinct leased cases = %d, want %d", len(seen), n)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM cases WHERE attempts = 1 AND status = 'open' AND leased_by IS NULL`), n; got != want {
		t.Fatalf("cases with one attempt = %d, want %d", got, want)
	}
}

// TestWorkerAttemptCapPrecheckEscalatesWithoutModelCall covers a case that is
// already at the cap when leased (for example after an expired-lease reclaim).
func TestWorkerAttemptCapPrecheckEscalatesWithoutModelCall(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 3)

	runner := &fakeRunner{}
	w := f.newWorker(t, f.workerID, runner, nil)
	if err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runs = %d, want 0", runner.callCount())
	}
	status, attempts, leasedBy, reason := f.caseRow(t, caseID)
	if status != "escalated" || attempts != 3 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want escalated/3/nil", status, attempts, leasedBy)
	}
	if reason == nil || *reason != "agent_failed" {
		t.Fatalf("escalation reason = %v, want agent_failed", reason)
	}
}

// TestWorkerLeaseExpiryReclaimCountsOnce proves an expired lease is reclaimed
// with one attempt increment and a late release/attempt cannot double-count.
func TestWorkerLeaseExpiryReclaimCountsOnce(t *testing.T) {
	ctx := context.Background()
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 0)

	leased, err := f.cases.Lease(ctx, f.workerID, time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if leased.ID != caseID {
		t.Fatalf("leased case = %s, want %s", leased.ID, caseID)
	}
	f.clock.Advance(2 * time.Minute)
	reclaimed, err := f.cases.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", reclaimed)
	}
	if _, err := f.cases.RecordFailedAttempt(ctx, caseID, f.workerID, 3); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("late attempt = %v, want ErrNotLeased", err)
	}
	if _, err := f.cases.Release(ctx, caseID, f.workerID); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("late release = %v, want ErrNotLeased", err)
	}
	status, attempts, leasedBy, _ := f.caseRow(t, caseID)
	if status != "open" || attempts != 1 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want open/1/nil", status, attempts, leasedBy)
	}
}

// TestWorkerGracefulShutdownReleasesLease cancels an in-flight run and proves
// the deferred release writes through a fresh context: the case is open with no
// attempt and the release is audited by the worker system actor.
func TestWorkerGracefulShutdownReleasesLease(t *testing.T) {
	f := newIntegrationFixture(t)
	caseID := f.insertCase(t, "open", 0)

	started := make(chan struct{}, 1)
	runner := &fakeRunner{fn: func(ctx context.Context, caseID string) (agent.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return agent.Result{RunID: "run-" + caseID}, ctx.Err()
	}}
	w := f.newWorker(t, f.workerID, runner, func(cfg *config.WorkerConfig) {
		cfg.ShutdownGrace = 100 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("run never started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}

	status, attempts, leasedBy, _ := f.caseRow(t, caseID)
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("case = %s attempts=%d leased_by=%v, want open/0/nil", status, attempts, leasedBy)
	}
	if got, want := f.count(t,
		`SELECT count(*) FROM audit_log WHERE entity_type = 'case' AND entity_id = $1 AND action = $2`,
		caseID, cases.ActionCaseRelease), 1; got != want {
		t.Fatalf("release audit rows = %d, want %d", got, want)
	}
	if got := f.count(t, `SELECT count(*) FROM agent_runs WHERE case_id = $1::uuid`, caseID); got != 0 {
		t.Fatalf("agent_runs for canceled run = %d, want 0", got)
	}
}
