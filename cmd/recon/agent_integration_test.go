//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/fixtures/m2"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the migration template used by the scripted CLI tests.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "recon CLI: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "recon CLI: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// cliFixture prepares a fresh database clone, loads the M2 fixture, points the
// CLI at the ephemeral role logins, and clears OPENCODE_API_KEY so every test
// proves that scripted replay works without a key.
func cliFixture(t *testing.T) *testdb.DB {
	t.Helper()
	d := testdb.New(t)
	if err := m2.Load(context.Background(), d.Pool); err != nil {
		t.Fatalf("load M2 fixture: %v", err)
	}
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", d.DSN)
	t.Setenv("DATABASE_APP_URL", d.DSNFor(db.RoleApp))
	t.Setenv("DATABASE_AGENT_URL", d.DSNFor(db.RoleAgentRO))
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_MODEL", "")
	t.Setenv("OPENCODE_BASE_URL", "")
	return d
}

// TestAgentCLIScriptedHumanOutput runs the documented scripted path with no
// API key and asserts the transcript names the case, prompt, outcome and
// proposal type without printing a key.
func TestAgentCLIScriptedHumanOutput(t *testing.T) {
	cliFixture(t)

	var out bytes.Buffer
	err := runAgentTo(context.Background(), []string{
		"run",
		"--case", m2.ReferenceTypoCaseID,
		"--llm", "scripted",
		"--prompt", "v1",
		"--cassette-dir", cliCassetteDir(t),
	}, &out)
	if err != nil {
		t.Fatalf("recon agent run: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	transcript := out.String()
	for _, want := range []string{
		"case: " + m2.ReferenceTypoCaseID,
		"prompt: v1",
		"outcome: proposal",
		"proposal:",
		"type=link",
		"status=proposed",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript does not contain %q:\n%s", want, transcript)
		}
	}
	if strings.Contains(transcript, "oc_sk_") || strings.Contains(transcript, "Bearer ") {
		t.Errorf("transcript leaks credentials:\n%s", transcript)
	}
}

// TestAgentCLIScriptedJSON runs the same replay path with --json and pins the
// machine-readable fields.
func TestAgentCLIScriptedJSON(t *testing.T) {
	cliFixture(t)

	var out bytes.Buffer
	err := runAgentTo(context.Background(), []string{
		"run",
		"--case", m2.FeeVarianceCaseID,
		"--llm", "scripted",
		"--prompt", "v1",
		"--cassette-dir", cliCassetteDir(t),
		"--json",
	}, &out)
	if err != nil {
		t.Fatalf("recon agent run --json: %v", err)
	}
	decoder := json.NewDecoder(&out)
	var report struct {
		CaseID       string `json:"case_id"`
		Outcome      string `json:"outcome"`
		ProposalType string `json:"proposal_type"`
		ProposalID   string `json:"proposal_id"`
		Steps        int    `json:"steps"`
		ToolCalls    int    `json:"tool_calls"`
	}
	if err := decoder.Decode(&report); err != nil {
		t.Fatalf("decode JSON report: %v\n%s", err, out.String())
	}
	if report.CaseID != m2.FeeVarianceCaseID {
		t.Errorf("case_id = %q, want %q", report.CaseID, m2.FeeVarianceCaseID)
	}
	if report.Outcome != "proposal" {
		t.Errorf("outcome = %q, want proposal", report.Outcome)
	}
	if report.ProposalType != "adjustment" {
		t.Errorf("proposal_type = %q, want adjustment", report.ProposalType)
	}
	if report.ProposalID == "" {
		t.Error("proposal_id is empty")
	}
	if report.Steps <= 0 || report.ToolCalls <= 0 {
		t.Errorf("steps/tool_calls = %d/%d, want positive", report.Steps, report.ToolCalls)
	}
	if strings.Contains(out.String(), "oc_sk_") {
		t.Errorf("JSON output leaks a key:\n%s", out.String())
	}
}

// TestAgentCLIRefusesLeasedCase pins that the CLI only runs a case it can
// lease: an in_agent case is refused before any run row is written.
func TestAgentCLIRefusesLeasedCase(t *testing.T) {
	d := cliFixture(t)

	actor, err := audit.SystemActor("cli-test")
	if err != nil {
		t.Fatalf("actor: %v", err)
	}
	svc, err := cases.NewService(d.Pool, clock.Real{}, actor)
	if err != nil {
		t.Fatalf("case service: %v", err)
	}
	if _, err := svc.LeaseByID(context.Background(), m2.DuplicateChargeCaseID, "test:holder:1", 3*time.Minute); err != nil {
		t.Fatalf("pre-lease case: %v", err)
	}

	var out bytes.Buffer
	err = runAgentTo(context.Background(), []string{
		"run",
		"--case", m2.DuplicateChargeCaseID,
		"--llm", "scripted",
		"--cassette-dir", cliCassetteDir(t),
	}, &out)
	if err == nil {
		t.Fatalf("recon agent run on leased case = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "lease case") || !strings.Contains(err.Error(), "in_agent") {
		t.Fatalf("error = %q, want a lease refusal naming in_agent", err)
	}
	var runs int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_runs WHERE case_id = $1::uuid`, m2.DuplicateChargeCaseID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("agent_runs for refused case = %d, want 0", runs)
	}
	if strings.Contains(out.String(), "outcome:") {
		t.Fatalf("refused case printed a transcript:\n%s", out.String())
	}
}

// TestAgentCLIDisabledSwitchExitsZero pins finding 4: a kill-switch stop is a
// persisted terminal outcome (run escalated, lease released), so the CLI
// reports it and exits 0 instead of treating it as an unpersisted error.
func TestAgentCLIDisabledSwitchExitsZero(t *testing.T) {
	d := cliFixture(t)
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE system_switches
		    SET enabled = false, reason = 'cli test', updated_by = 'test:cli', updated_at = now()
		  WHERE name = 'agent_enabled'`); err != nil {
		t.Fatalf("disable agent switch: %v", err)
	}

	var out bytes.Buffer
	err := runAgentTo(context.Background(), []string{
		"run",
		"--case", m2.DuplicateChargeCaseID,
		"--llm", "scripted",
		"--prompt", "v1",
	}, &out)
	if err != nil {
		t.Fatalf("recon agent run with switch off: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	transcript := out.String()
	if !strings.Contains(transcript, "outcome: escalated") {
		t.Fatalf("transcript does not report the escalated outcome:\n%s", transcript)
	}

	var (
		outcome string
		endedAt *time.Time
	)
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT outcome, ended_at FROM agent_runs WHERE case_id = $1::uuid`,
		m2.DuplicateChargeCaseID).Scan(&outcome, &endedAt); err != nil {
		t.Fatalf("read disabled run: %v", err)
	}
	if outcome != "escalated" || endedAt == nil {
		t.Fatalf("run outcome/ended = %s/%v, want escalated/non-nil", outcome, endedAt)
	}
	var (
		status   string
		attempts int
		leasedBy *string
	)
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT status, attempts, leased_by FROM cases WHERE id = $1::uuid`,
		m2.DuplicateChargeCaseID).Scan(&status, &attempts, &leasedBy); err != nil {
		t.Fatalf("read disabled case: %v", err)
	}
	if status != "open" || attempts != 0 || leasedBy != nil {
		t.Fatalf("case = status %s attempts %d leased_by %v, want open/0/nil",
			status, attempts, leasedBy)
	}
}

func cliCassetteDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "eval", "cassettes")
}
