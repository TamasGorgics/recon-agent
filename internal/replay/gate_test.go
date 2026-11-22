//go:build integration

package replay_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const gateCaseCount = 20

// gateCase is one seeded case and its expected recorded proposal.
type gateCase struct {
	caseID       string
	runID        string
	scenario     string
	expectedType string
}

// covers: FIN-43
// TestTwentyRunExactReplayGate is the M5-02 acceptance gate (FIN-43): twenty
// seeded cases over the three M2 scenarios (7/7/6) run through the production
// agent loop with the committed deterministic script, are recorded through the
// real recorder, loaded once in read-only transactions, and then replayed
// exactly with every pool closed. The gate requires 20/20 identical.
func TestTwentyRunExactReplayGate(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	if err := m2.Load(ctx, d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	cfg := replayTestConfig()
	script := loadScenarioScript(t)
	gateCases := seedGateCases(t, ctx, d)

	app := openRolePool(t, d, db.RoleApp)
	agentRO := openRolePool(t, d, db.RoleAgentRO)

	seenRunIDs := make(map[string]struct{}, len(gateCases))
	for _, gate := range gateCases {
		client := script.scriptedClient(t, gate.scenario, gate.caseID, cfg)
		report, err := agentrun.Run(ctx, agentrun.Options{
			CaseID:        gate.caseID,
			Mode:          agentrun.ModeScripted,
			PromptVersion: cfg.Agent.PromptVersion,
		}, agentrun.Deps{
			Config:    cfg,
			App:       app,
			Agent:     agentRO,
			AgentExec: agentRO,
			Clock:     clock.Real{},
			Logger:    discardLogger(),
			Client:    client,
			NewRunID:  agent.DeterministicRunID,
			WorkerID:  "test:m5-02-gate",
		})
		if err != nil {
			t.Fatalf("record %s/%s: %v", gate.scenario, gate.caseID, err)
		}
		if report.RunID != gate.runID {
			t.Fatalf("record %s: run id = %q, want deterministic %q", gate.caseID, report.RunID, gate.runID)
		}
		if report.Outcome != string(agent.OutcomeProposal) {
			t.Fatalf("record %s: outcome = %q, want proposal", gate.caseID, report.Outcome)
		}
		if report.ProposalType != gate.expectedType {
			t.Fatalf("record %s: proposal type = %q, want %q", gate.caseID, report.ProposalType, gate.expectedType)
		}
		steps := len(script.Scenarios[gate.scenario].Steps)
		if client.Calls() != steps || client.Remaining() != 0 {
			t.Fatalf("record %s: scripted calls = %d remaining = %d, want %d/0",
				gate.caseID, client.Calls(), client.Remaining(), steps)
		}
		if _, duplicate := seenRunIDs[report.RunID]; duplicate {
			t.Fatalf("run id %s was recorded twice", report.RunID)
		}
		seenRunIDs[report.RunID] = struct{}{}
		assertRecordedGateRun(t, ctx, d, gate, steps)
	}
	if len(seenRunIDs) != gateCaseCount {
		t.Fatalf("recorded %d distinct runs, want %d", len(seenRunIDs), gateCaseCount)
	}

	// The load phase: one REPEATABLE READ READ ONLY transaction per run. After
	// all 20 are loaded, both role pools are closed before any replay.
	recordings := make([]replay.Recording, 0, len(gateCases))
	for _, gate := range gateCases {
		rec := loadRecordingReadOnly(t, ctx, app, gate.runID)
		if rec.Run.Outcome != string(agent.OutcomeProposal) || rec.Proposal == nil {
			t.Fatalf("load %s: outcome = %q proposal = %v", gate.caseID, rec.Run.Outcome, rec.Proposal)
		}
		recordings = append(recordings, rec)
	}

	app.Close()
	agentRO.Close()
	var one int
	if err := app.QueryRow(ctx, `SELECT 1`).Scan(&one); err == nil {
		t.Fatal("closed app pool still answered a query before replay")
	}
	if err := agentRO.QueryRow(ctx, `SELECT 1`).Scan(&one); err == nil {
		t.Fatal("closed agent pool still answered a query before replay")
	}

	replayConfig := replay.Config{
		Provider: cfg.LLM.Provider,
		BaseURL:  cfg.LLM.BaseURL,
		Agent:    cfg.Agent,
	}
	identical := 0
	for index, rec := range recordings {
		gate := gateCases[index]
		report, err := replay.ExactReplay(rec, replayConfig)
		if err != nil {
			t.Fatalf("replay %02d/20 %s: %v", index+1, gate.caseID, err)
		}
		if report.Identical {
			identical++
		}
		if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
			t.Errorf("replay %02d/20 %s: proposal_hash = %q, stored_hash = %q",
				index+1, gate.caseID, report.ProposalHash, report.StoredHash)
		}
		if len(report.Divergences) != 0 {
			t.Errorf("replay %02d/20 %s: divergences = %+v", index+1, gate.caseID, report.Divergences)
		}
		if report.StepsTotal != len(rec.LLMCalls) {
			t.Errorf("replay %02d/20 %s: steps_total = %d, want %d",
				index+1, gate.caseID, report.StepsTotal, len(rec.LLMCalls))
		}
		assertGateClock(t, rec, report.Clock)
		t.Logf("M5-02 gate run %02d/20: scenario=%s case=%s run=%s identical=%t steps=%d tools=%d proposal_hash=%s",
			index+1, gate.scenario, gate.caseID, gate.runID, report.Identical,
			len(rec.LLMCalls), len(rec.ToolCalls), report.ProposalHash)
	}
	if identical != gateCaseCount {
		t.Fatalf("20-run gate: %d/%d exact-identical, want 20/20", identical, gateCaseCount)
	}
	t.Logf("M5-02 20-run gate: %d/20 exact-identical over scenarios fee_variance=7 reference_typo=7 duplicate_charge=6",
		identical)
}

// seedGateCases inserts 20 distinct cases over three scenarios and clones one
// fixture break per case. break.id and break.signature as well as break_id in
// case_breaks are unique, so every case gets its own copied break row with the
// fixture ledger transaction ids and external refs.
func seedGateCases(t *testing.T, ctx context.Context, d *testdb.DB) []gateCase {
	t.Helper()
	plans := []struct {
		scenario     string
		fixtureCase  string
		fixtureBreak string
		expectedType string
		count        int
	}{
		{m2.ScenarioFeeVariance, m2.FeeVarianceCaseID, "b0000000-0000-4000-8000-000000000001", "adjustment", 7},
		{m2.ScenarioReferenceTypo, m2.ReferenceTypoCaseID, "b0000000-0000-4000-8000-000000000002", "link", 7},
		{m2.ScenarioDuplicate, m2.DuplicateChargeCaseID, "b0000000-0000-4000-8000-000000000003", "duplicate", 6},
	}

	cases := make([]gateCase, 0, gateCaseCount)
	err := db.WithTx(ctx, d.Pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		number := 0
		for _, plan := range plans {
			for i := 0; i < plan.count; i++ {
				number++
				caseID := fmt.Sprintf("20000000-0000-4000-8000-%012d", number)
				breakID := fmt.Sprintf("b1000000-0000-4000-8000-%012d", number)
				signature := gateSignature(plan.scenario, number)

				tag, err := tx.Exec(ctx,
					`INSERT INTO breaks
					     (id, signature, scope_id, match_run_id, side, ledger_txn_ids,
					      external_refs, delta_minor, currency, status, detected_at)
					 SELECT $1::uuid, $2, scope_id, match_run_id, side, ledger_txn_ids,
					        external_refs, delta_minor, currency, 'in_case', detected_at
					   FROM breaks WHERE id = $3::uuid`,
					breakID, signature, plan.fixtureBreak)
				if err != nil {
					return fmt.Errorf("clone break %d: %w", number, err)
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("clone break %d: fixture break %s not found", number, plan.fixtureBreak)
				}

				tag, err = tx.Exec(ctx,
					`INSERT INTO cases
					     (id, priority, value_minor, currency, status, version,
					      attempts, created_at)
					 SELECT $1::uuid, priority, value_minor, currency, 'open', 0, 0, created_at
					   FROM cases WHERE id = $2::uuid`,
					caseID, plan.fixtureCase)
				if err != nil {
					return fmt.Errorf("clone case %d: %w", number, err)
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("clone case %d: fixture case %s not found", number, plan.fixtureCase)
				}

				if _, err := tx.Exec(ctx,
					`INSERT INTO case_breaks (case_id, break_id, created_at)
					 VALUES ($1::uuid, $2::uuid, $3)`,
					caseID, breakID, m2.FixedInstant); err != nil {
					return fmt.Errorf("link case %d: %w", number, err)
				}

				runID, err := agent.DeterministicRunID(caseID)
				if err != nil {
					return fmt.Errorf("derive run id %d: %w", number, err)
				}
				cases = append(cases, gateCase{
					caseID:       caseID,
					runID:        runID,
					scenario:     plan.scenario,
					expectedType: plan.expectedType,
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed 20 gate cases: %v", err)
	}
	if len(cases) != gateCaseCount {
		t.Fatalf("seeded %d gate cases, want %d", len(cases), gateCaseCount)
	}
	return cases
}

func gateSignature(scenario string, number int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("m5-02-gate:%s:%d", scenario, number)))
	return hex.EncodeToString(sum[:])
}

// assertRecordedGateRun proves the run went through the real recorder: the
// expected llm_calls/tool_calls counts and a non-NULL canonical hash on every
// new tool call.
func assertRecordedGateRun(t *testing.T, ctx context.Context, d *testdb.DB, gate gateCase, steps int) {
	t.Helper()
	var (
		outcome      string
		proposalType string
		llmCalls     int
		toolCalls    int
		canonical    int
	)
	if err := d.Pool.QueryRow(ctx,
		`SELECT r.outcome, p.type,
		        (SELECT count(*) FROM llm_calls l WHERE l.agent_run_id = r.id),
		        (SELECT count(*) FROM tool_calls c WHERE c.agent_run_id = r.id),
		        (SELECT count(*) FROM tool_calls c
		          WHERE c.agent_run_id = r.id AND c.result_canonical_hash IS NOT NULL)
		   FROM agent_runs r
		   JOIN proposals p ON p.agent_run_id = r.id
		  WHERE r.id = $1::uuid`, gate.runID).
		Scan(&outcome, &proposalType, &llmCalls, &toolCalls, &canonical); err != nil {
		t.Fatalf("read recorded run %s: %v", gate.runID, err)
	}
	if outcome != string(agent.OutcomeProposal) {
		t.Fatalf("%s: recorded outcome = %q, want proposal", gate.caseID, outcome)
	}
	if proposalType != gate.expectedType {
		t.Fatalf("%s: recorded proposal type = %q, want %q", gate.caseID, proposalType, gate.expectedType)
	}
	if llmCalls != steps {
		t.Fatalf("%s: llm_calls = %d, want %d", gate.caseID, llmCalls, steps)
	}
	if toolCalls != steps+1 {
		t.Fatalf("%s: tool_calls = %d, want %d (seed + one per step)", gate.caseID, toolCalls, steps+1)
	}
	if canonical != toolCalls {
		t.Fatalf("%s: %d/%d tool calls have result_canonical_hash, want all", gate.caseID, canonical, toolCalls)
	}
}

// assertGateClock proves replay's fake clock values come from the recorded
// timestamps and durations for every step.
func assertGateClock(t *testing.T, rec replay.Recording, events []replay.ClockEvent) {
	t.Helper()
	if len(events) != len(rec.LLMCalls)+len(rec.ToolCalls) {
		t.Fatalf("clock events = %d, want %d", len(events), len(rec.LLMCalls)+len(rec.ToolCalls))
	}
	llmSeen, toolSeen := 0, 0
	for _, event := range events {
		switch event.Kind {
		case "llm":
			if event.Step < 1 || event.Step > len(rec.LLMCalls) {
				t.Fatalf("clock llm event has step %d", event.Step)
			}
			recorded := rec.LLMCalls[event.Step-1]
			if !event.At.Equal(recorded.StartedAt) || event.DurationMS != recorded.DurationMS {
				t.Fatalf("clock llm step %d = %s/%dms, want %s/%dms",
					event.Step, event.At, event.DurationMS, recorded.StartedAt, recorded.DurationMS)
			}
			llmSeen++
		case "tool":
			if event.Tool < 1 || event.Tool > len(rec.ToolCalls) {
				t.Fatalf("clock tool event has tool seq %d", event.Tool)
			}
			recorded := rec.ToolCalls[event.Tool-1]
			// Replay does not derive any time value from a tool call, so only
			// the recorded instant is replayed for tool events.
			if !event.At.Equal(recorded.StartedAt) {
				t.Fatalf("clock tool seq %d = %s, want recorded %s",
					event.Tool, event.At, recorded.StartedAt)
			}
			toolSeen++
		default:
			t.Fatalf("clock event has unknown kind %q", event.Kind)
		}
	}
	if llmSeen != len(rec.LLMCalls) || toolSeen != len(rec.ToolCalls) {
		t.Fatalf("clock covered %d llm / %d tool events, want %d/%d",
			llmSeen, toolSeen, len(rec.LLMCalls), len(rec.ToolCalls))
	}
}
