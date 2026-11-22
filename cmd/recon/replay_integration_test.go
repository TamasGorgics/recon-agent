//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/replay"
)

// TestReplayCLIExactFromDatabase records one run through the production loop
// and cassette path, then replays it through the real CLI load phase: one
// REPEATABLE READ READ ONLY transaction on the app role, the pool closed before
// ExactReplay, and exit 0 with the full JSON report.
func TestReplayCLIExactFromDatabase(t *testing.T) {
	cliFixture(t)
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		t.Fatalf("open app role: %v", err)
	}
	agentRO, err := db.OpenRole(ctx, db.RoleAgentRO, cfg.Database.AgentURL)
	if err != nil {
		app.Close()
		t.Fatalf("open agent role: %v", err)
	}
	recorded, err := agentrun.Run(ctx, agentrun.Options{
		CaseID:        m2.ReferenceTypoCaseID,
		Mode:          agentrun.ModeScripted,
		PromptVersion: "v1",
		CassetteDir:   cliCassetteDir(t),
	}, agentrun.Deps{
		Config:    cfg,
		App:       app.Pool,
		Agent:     agentRO.Pool,
		AgentExec: agentRO.Pool,
		Clock:     clock.Real{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRunID:  agent.DeterministicRunID,
		WorkerID:  "test:replay-cli",
	})
	app.Close()
	agentRO.Close()
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if recorded.Outcome != string(agent.OutcomeProposal) {
		t.Fatalf("recorded outcome = %q, want proposal", recorded.Outcome)
	}

	var out bytes.Buffer
	err = runReplayTo(ctx, []string{"--run", recorded.RunID, "--exact", "--json"}, &out, replayDeps{})
	if err != nil {
		t.Fatalf("recon replay --exact: %v\n%s", err, out.String())
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var report replay.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if !report.Identical || report.Mode != replay.ModeExact {
		t.Fatalf("report = %+v, want an identical exact replay", report)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
}

// TestReplayCLIRunNotFoundFromDatabase proves the real load phase maps a
// missing run to exit 2 run_not_found with no stdout.
func TestReplayCLIRunNotFoundFromDatabase(t *testing.T) {
	cliFixture(t)

	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", "99999999-9999-4999-8999-999999999999", "--exact", "--json",
	}, &out, replayDeps{})
	if err == nil {
		t.Fatal("recon replay for a missing run = nil, want an error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(err.Error(), "run_not_found") {
		t.Fatalf("error = %q, want run_not_found", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing for exit 2", out.String())
	}
}
