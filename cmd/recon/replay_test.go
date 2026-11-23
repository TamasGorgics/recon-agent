package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
	"github.com/TamasGorgics/recon-agent/prompts"
)

const (
	cliReplayRunID  = "44444444-4444-4444-8444-444444444444"
	cliReplayCaseID = "55555555-5555-4555-8555-555555555555"
	cliReplayPrompt = "v1"
	cliStateHash    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	cliSeedTime     = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	cliLLMTime      = time.Date(2026, 4, 1, 9, 0, 5, 0, time.UTC)
	cliProposalTime = time.Date(2026, 4, 1, 9, 0, 7, 0, time.UTC)
)

// cliReplayFixture is a complete synthetic recording plus the exact step-1
// request and response. The request is what a cassette recording needs; the
// response is what the recorded engine replays.
type cliReplayFixture struct {
	Recording replay.Recording
	Request   llm.Request
	Response  llm.Response
}

// prepareReplayEnv makes config.Load deterministic: no config file, no role
// DSNs to be accidentally used, and no model or key overrides.
func prepareReplayEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CONFIG_FILE",
		"DATABASE_URL",
		"DATABASE_APP_URL",
		"DATABASE_AGENT_URL",
		"TEST_DATABASE_URL",
		"OPENCODE_API_KEY",
		"OPENCODE_MODEL",
		"OPENCODE_BASE_URL",
		"LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

// newCLIReplayFixture mirrors the production recording path (the same helper
// the internal replay tests use) so a mutation isolates exactly one rule.
func newCLIReplayFixture(t *testing.T) cliReplayFixture {
	t.Helper()
	agentConfig := config.DefaultAgentConfig()
	agentConfig.Model = config.DefaultLLMModel
	agentConfig.PromptVersion = cliReplayPrompt
	prompt, err := prompts.Load(cliReplayPrompt)
	if err != nil {
		t.Fatalf("load prompt: %v", err)
	}
	toolDefs, err := agent.ReplayToolDefinitions()
	if err != nil {
		t.Fatalf("tool definitions: %v", err)
	}
	wrapper, err := untrusted.NewRunWrapper(cliReplayRunID)
	if err != nil {
		t.Fatalf("run wrapper: %v", err)
	}

	seedArgs := cliMustJSON(t, map[string]any{"case_id": cliReplayCaseID})
	seedEnvelope := cliMustJSON(t, map[string]any{
		"data":             map[string]any{"id": cliReplayCaseID, "status": "in_agent"},
		"refs":             []string{"case:" + cliReplayCaseID},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
	seedHash, err := tools.CanonicalJSONHash(seedEnvelope)
	if err != nil {
		t.Fatalf("seed canonical hash: %v", err)
	}

	proposalArgs := cliMustJSON(t, map[string]any{
		"case_id":    cliReplayCaseID,
		"rationale":  "Need the settlement reference before deciding.",
		"evidence":   []string{"case:" + cliReplayCaseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
	proposalEnvelope := cliMustJSON(t, map[string]any{
		"data":             map[string]any{"status": "recorded"},
		"refs":             []string{},
		"untrusted_fields": []string{},
		"truncated":        false,
	})
	proposalResultHash, err := tools.CanonicalJSONHash(proposalEnvelope)
	if err != nil {
		t.Fatalf("proposal canonical hash: %v", err)
	}

	fields, err := proposal.Rebuild("request_info", proposalArgs, cliReplayCaseID)
	if err != nil {
		t.Fatalf("rebuild proposal: %v", err)
	}
	key, err := proposal.IdempotencyKey(cliReplayCaseID, cliReplayRunID, fields)
	if err != nil {
		t.Fatalf("idempotency key: %v", err)
	}
	payload, err := json.Marshal(fields.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evidence, err := json.Marshal(fields.Evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}

	seedVisible, err := agent.ModelVisibleResult(seedEnvelope, wrapper)
	if err != nil {
		t.Fatalf("wrap seed: %v", err)
	}
	messages := agent.InitialTranscript(cliReplayCaseID, seedVisible)
	temperature := agentConfig.TemperatureValue()
	request := llm.Request{
		Model:         agentConfig.Model,
		System:        prompt.Composed,
		Messages:      messages,
		Tools:         toolDefs,
		ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		Temperature:   &temperature, //floatlint:allow provider temperature, not money
		MaxTokens:     agentConfig.MaxTokensPerResponse,
		SessionID:     "ses_" + strings.ReplaceAll(cliReplayRunID, "-", ""),
		PromptVersion: cliReplayPrompt,
	}
	requestHash, err := llm.CanonicalRequestHash(config.DefaultLLMProvider, config.DefaultLLMBaseURL, request)
	if err != nil {
		t.Fatalf("request hash: %v", err)
	}
	response := llm.Response{
		Provider:       config.DefaultLLMProvider,
		RequestedModel: llm.APIID(agentConfig.Model),
		ActualModel:    agentConfig.Model,
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID:        "call_1",
				Name:      "request_info",
				Arguments: proposalArgs,
			}},
		},
		StopReason: llm.StopToolCalls,
	}
	responseBody, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	usageBody, err := json.Marshal(response.Usage)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}

	return cliReplayFixture{
		Request:  request,
		Response: response,
		Recording: replay.Recording{
			Run: replay.Run{
				ID:            cliReplayRunID,
				CaseID:        cliReplayCaseID,
				Model:         agentConfig.Model,
				PromptVersion: cliReplayPrompt,
				Outcome:       "proposal",
				StartedAt:     cliSeedTime,
			},
			LLMCalls: []replay.LLMCall{{
				Seq:         1,
				Model:       agentConfig.Model,
				RequestHash: requestHash,
				Response:    responseBody,
				Usage:       usageBody,
				StartedAt:   cliLLMTime,
				DurationMS:  5,
			}},
			ToolCalls: []replay.ToolCall{
				{
					Seq:                 1,
					ToolName:            "get_case",
					Args:                seedArgs,
					Result:              seedEnvelope,
					ResultCanonicalHash: &seedHash,
					StartedAt:           cliSeedTime,
				},
				{
					Seq:                 2,
					ToolName:            "request_info",
					Args:                proposalArgs,
					Result:              proposalEnvelope,
					ResultCanonicalHash: &proposalResultHash,
					StartedAt:           cliProposalTime,
				},
			},
			Proposal: &replay.Proposal{
				ID:             "66666666-6666-4666-8666-666666666666",
				Type:           fields.Type,
				Payload:        payload,
				Rationale:      fields.Rationale,
				Evidence:       evidence,
				Confidence:     fields.Confidence,
				PatternClaimed: fields.PatternClaimed,
				StateHash:      cliStateHash,
				IdempotencyKey: key,
			},
		},
	}
}

func cliMustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// replayLoader returns a load seam that records its call count.
func replayLoader(t *testing.T, recording replay.Recording, calls *int) func(context.Context, config.Config, string) (replay.Recording, error) {
	t.Helper()
	return func(_ context.Context, _ config.Config, runID string) (replay.Recording, error) {
		*calls++
		if runID != cliReplayRunID {
			t.Fatalf("load run id = %q, want %q", runID, cliReplayRunID)
		}
		return recording, nil
	}
}

func decodeCLIReplayReport(t *testing.T, out []byte) replay.Report {
	t.Helper()
	var report replay.Report
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out)
	}
	return report
}

// TestReplayCLIExactIdenticalJSON pins the exact-success path: one load, exit 0,
// and the required top-level JSON fields.
func TestReplayCLIExactIdenticalJSON(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err != nil {
		t.Fatalf("recon replay --exact: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if loads != 1 {
		t.Fatalf("load calls = %d, want exactly 1", loads)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &top); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, out.String())
	}
	for _, key := range []string{"run_id", "mode", "identical", "proposal_hash", "stored_hash", "divergences"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("JSON is missing required field %q:\n%s", key, out.String())
		}
	}
	var divergences []replay.Divergence
	if err := json.Unmarshal(top["divergences"], &divergences); err != nil || divergences == nil {
		t.Fatalf("divergences = %s, want an empty array", top["divergences"])
	}
	if len(divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", divergences)
	}

	report := decodeCLIReplayReport(t, out.Bytes())
	if report.RunID != cliReplayRunID || report.Mode != replay.ModeExact || !report.Identical {
		t.Fatalf("report = %+v, want the identical exact run", report)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
	if !strings.HasSuffix(out.String(), "\n") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("JSON output is not one line:\n%s", out.String())
	}
}

// TestReplayCLIExactDivergenceReportsOnStdout proves a request-hash or proposal
// field divergence exits 1 and still prints the full report to stdout in both
// JSON and human modes.
func TestReplayCLIExactDivergenceReportsOnStdout(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		prepareReplayEnv(t)
		fixture := newCLIReplayFixture(t)
		fixture.Recording.LLMCalls[0].RequestHash = strings.Repeat("a", 64)
		loads := 0
		var out bytes.Buffer
		err := runReplayTo(context.Background(), []string{
			"--run", cliReplayRunID, "--exact", "--json",
		}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
		if err == nil {
			t.Fatal("recon replay returned nil for a divergence")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		report := decodeCLIReplayReport(t, out.Bytes())
		if report.Identical {
			t.Fatal("identical = true for a request hash divergence")
		}
		if len(report.Divergences) != 1 {
			t.Fatalf("divergences = %+v, want one", report.Divergences)
		}
		divergence := report.Divergences[0]
		if divergence.Code != replay.CodeRequestHashMismatch || divergence.Step != 1 {
			t.Fatalf("divergence = %+v, want request_hash_mismatch at step 1", divergence)
		}
	})

	t.Run("proposal field", func(t *testing.T) {
		prepareReplayEnv(t)
		fixture := newCLIReplayFixture(t)
		fixture.Recording.Proposal.Payload = cliMustJSON(t, map[string]any{
			"question": "A different stored question",
		})
		loads := 0
		var out bytes.Buffer
		err := runReplayTo(context.Background(), []string{
			"--run", cliReplayRunID, "--exact", "--json",
		}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
		if err == nil || exitCode(err) != 1 {
			t.Fatalf("error = %v, want exit 1", err)
		}
		report := decodeCLIReplayReport(t, out.Bytes())
		if len(report.Divergences) != 1 ||
			report.Divergences[0].Code != replay.CodeProposalDigestMismatch ||
			report.Divergences[0].Step != 0 {
			t.Fatalf("divergences = %+v, want proposal_digest_mismatch at step 0", report.Divergences)
		}
	})

	t.Run("human", func(t *testing.T) {
		prepareReplayEnv(t)
		fixture := newCLIReplayFixture(t)
		fixture.Recording.LLMCalls[0].RequestHash = strings.Repeat("a", 64)
		loads := 0
		var out bytes.Buffer
		err := runReplayTo(context.Background(), []string{
			"--run", cliReplayRunID, "--exact",
		}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
		if err == nil || exitCode(err) != 1 {
			t.Fatalf("error = %v, want exit 1", err)
		}
		human := out.String()
		for _, want := range []string{"DIVERGED", "run=" + cliReplayRunID, "request_hash_mismatch", "step=1"} {
			if !strings.Contains(human, want) {
				t.Errorf("human report does not contain %q:\n%s", want, human)
			}
		}
	})
}

// TestReplayCLIKeyOnlyDiffExitsOne proves the exit status follows the
// divergence list, not `identical`: a key-only proposal_field_diff keeps
// identical=true but still exits 1 with the complete report on stdout.
func TestReplayCLIKeyOnlyDiffExitsOne(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	fixture.Recording.Proposal.IdempotencyKey = strings.Repeat("c", 64)
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err == nil || exitCode(err) != 1 {
		t.Fatalf("error = %v, want exit 1", err)
	}
	report := decodeCLIReplayReport(t, out.Bytes())
	if !report.Identical {
		t.Fatalf("identical = false for a key-only diff, divergences = %+v", report.Divergences)
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one", report.Divergences)
	}
	divergence := report.Divergences[0]
	if divergence.Code != replay.CodeProposalFieldDiff || divergence.Step != 0 ||
		divergence.Field == nil || *divergence.Field != "idempotency_key" {
		t.Fatalf("divergence = %+v, want proposal_field_diff(idempotency_key)", divergence)
	}
	if report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q, want equal", report.ProposalHash, report.StoredHash)
	}
}

// TestReplayCLIUsageErrorsExit2 pins exit 2 for every usage error, with no
// stdout output and no load call.
func TestReplayCLIUsageErrorsExit2(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing run", args: []string{"--exact"}, want: "--run <uuid> is required"},
		{name: "invalid run", args: []string{"--run", "not-a-uuid", "--exact"}, want: "is not a UUID"},
		{name: "both modes", args: []string{"--run", cliReplayRunID, "--exact", "--rerun"}, want: "mutually exclusive"},
		{name: "no mode", args: []string{"--run", cliReplayRunID}, want: "exactly one of --exact or --rerun"},
		{name: "unexpected argument", args: []string{"--run", cliReplayRunID, "--exact", "extra"}, want: "unexpected argument"},
		{name: "exact override", args: []string{"--run", cliReplayRunID, "--exact", "--model", "x"}, want: "apply only to --rerun"},
		{name: "unknown flag", args: []string{"--run", cliReplayRunID, "--exact", "--nope"}, want: "flag provided but not defined"},
		{name: "unknown prompt", args: []string{"--run", cliReplayRunID, "--rerun", "--prompt", "v99"}, want: "unknown version"},
		{name: "live without key", args: []string{"--run", cliReplayRunID, "--rerun", "--live"}, want: "live_key_missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepareReplayEnv(t)
			loads := 0
			var out bytes.Buffer
			err := runReplayTo(context.Background(), test.args, &out, replayDeps{
				loadRecording: func(context.Context, config.Config, string) (replay.Recording, error) {
					loads++
					return replay.Recording{}, errors.New("load must not run for a usage error")
				},
			})
			if err == nil {
				t.Fatal("recon replay = nil, want a usage error")
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing for exit 2", out.String())
			}
			if loads != 0 {
				t.Fatalf("load calls = %d, want 0 for a usage error", loads)
			}
		})
	}
}

// TestReplayCLILoadErrorsExit2 pins the load-phase mapping, including that
// --json never receives a partial object.
func TestReplayCLILoadErrorsExit2(t *testing.T) {
	tests := []struct {
		name    string
		loadErr error
		want    string
	}{
		{name: "run not found", loadErr: fmt.Errorf("%w: %s", replay.ErrRunNotFound, cliReplayRunID), want: "run_not_found"},
		{
			name: "unsupported run",
			loadErr: &replay.UnsupportedRunError{
				RunID: cliReplayRunID, Outcome: "escalated", Mode: replay.ModeExact,
			},
			want: "unsupported_run",
		},
		{name: "database error", loadErr: errors.New("connection refused"), want: "replay: load:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepareReplayEnv(t)
			var out bytes.Buffer
			err := runReplayTo(context.Background(), []string{
				"--run", cliReplayRunID, "--exact", "--json",
			}, &out, replayDeps{
				loadRecording: func(context.Context, config.Config, string) (replay.Recording, error) {
					return replay.Recording{}, test.loadErr
				},
			})
			if err == nil {
				t.Fatal("recon replay = nil, want a load error")
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout for exit 2 --json = %q, want nothing", out.String())
			}
		})
	}
}

// TestReplayCLIRerunIdentical proves the rerun success path through the CLI,
// including that the injected client is the cassette-shaped (non-live) one and
// that a stored idempotency-key difference does not change identity (A10).
func TestReplayCLIRerunIdentical(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	fixture.Recording.Proposal.IdempotencyKey = strings.Repeat("c", 64)
	loads := 0
	var clientOpts replayClientOptions
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--rerun", "--json",
	}, &out, replayDeps{
		loadRecording: replayLoader(t, fixture.Recording, &loads),
		newClient: func(_ config.Config, opts replayClientOptions) (llm.Client, error) {
			clientOpts = opts
			return llm.NewScripted(llm.ScriptedStep{Response: fixture.Response}), nil
		},
	})
	if err != nil {
		t.Fatalf("recon replay --rerun: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if clientOpts.Live {
		t.Fatal("rerun default constructed a live client")
	}
	if clientOpts.CassetteDir != llm.DefaultCassetteDir {
		t.Fatalf("cassette dir = %q, want %q", clientOpts.CassetteDir, llm.DefaultCassetteDir)
	}
	report := decodeCLIReplayReport(t, out.Bytes())
	if report.Mode != replay.ModeRerun || !report.Identical {
		t.Fatalf("report = %+v, want an identical rerun", report)
	}
}

// TestReplayCLIRerunDivergence proves a model/prompt override that changes the
// proposal exits 1 with the JSON report on stdout.
func TestReplayCLIRerunDivergence(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	response := fixture.Response
	response.Message.ToolCalls[0].Arguments = cliMustJSON(t, map[string]any{
		"case_id":    cliReplayCaseID,
		"rationale":  "A different rationale from the rerun.",
		"evidence":   []string{"case:" + cliReplayCaseID},
		"confidence": 0.5,
		"question":   "Which settlement reference matches this case?",
	})
	loads := 0
	var clientOpts replayClientOptions
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--rerun", "--prompt", "v2", "--json",
	}, &out, replayDeps{
		loadRecording: replayLoader(t, fixture.Recording, &loads),
		newClient: func(_ config.Config, opts replayClientOptions) (llm.Client, error) {
			clientOpts = opts
			return llm.NewScripted(llm.ScriptedStep{Response: response}), nil
		},
	})
	if err == nil {
		t.Fatal("recon replay --rerun returned nil for a divergence")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if clientOpts.Live {
		t.Fatal("rerun with --prompt constructed a live client")
	}
	if clientOpts.Prompt != "v2" {
		t.Fatalf("client prompt override = %q, want v2", clientOpts.Prompt)
	}
	report := decodeCLIReplayReport(t, out.Bytes())
	if report.Mode != replay.ModeRerun || report.Identical {
		t.Fatalf("report = %+v, want a diverged rerun", report)
	}
	if len(report.Divergences) != 1 || report.Divergences[0].Code != replay.CodeProposalDigestMismatch {
		t.Fatalf("divergences = %+v, want proposal_digest_mismatch", report.Divergences)
	}
	if report.Prompt != "v2" {
		t.Fatalf("report prompt = %q, want v2", report.Prompt)
	}
}

// TestReplayCLICassetteDefault proves the no-flag rerun path uses the cassette
// directory: a recorded cassette replays to identical, and an empty directory
// is an exit-2 cassette miss, never a divergence.
func TestReplayCLICassetteDefault(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	dir := t.TempDir()

	record, err := llm.NewCassette(llm.NewScripted(llm.ScriptedStep{Response: fixture.Response}), llm.CassetteOptions{
		Mode:     llm.CassetteRecord,
		Dir:      dir,
		Provider: config.DefaultLLMProvider,
		BaseURL:  config.DefaultLLMBaseURL,
	})
	if err != nil {
		t.Fatalf("recording cassette: %v", err)
	}
	if _, err := record.Complete(context.Background(), fixture.Request); err != nil {
		t.Fatalf("record cassette: %v", err)
	}

	loads := 0
	var out bytes.Buffer
	err = runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--rerun", "--cassette-dir", dir, "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err != nil {
		t.Fatalf("recon replay --rerun from cassette: %v", err)
	}
	report := decodeCLIReplayReport(t, out.Bytes())
	if !report.Identical {
		t.Fatalf("cassette rerun identical = false: %+v", report.Divergences)
	}

	loads = 0
	out.Reset()
	err = runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--rerun", "--cassette-dir", t.TempDir(), "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err == nil {
		t.Fatal("recon replay --rerun with an empty cassette dir = nil, want a miss")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("cassette miss exit code = %d, want 2", code)
	}
	if !strings.Contains(err.Error(), "cassette_miss") {
		t.Fatalf("error = %q, want cassette_miss", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout for a cassette miss = %q, want nothing", out.String())
	}
}

// TestReplayCLIRejectsUnsupportedExactRun proves the CLI maps an unsupported
// exact outcome (no fabricated identity).
func TestReplayCLIRejectsUnsupportedExactRun(t *testing.T) {
	prepareReplayEnv(t)
	fixture := newCLIReplayFixture(t)
	fixture.Recording.Run.Outcome = "provider_error"
	fixture.Recording.Proposal = nil
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err == nil || exitCode(err) != 2 {
		t.Fatalf("error = %v, want exit 2", err)
	}
	if !strings.Contains(err.Error(), "unsupported_run") || !strings.Contains(err.Error(), "provider_error") {
		t.Fatalf("error = %q, want unsupported_run naming provider_error", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing for exit 2", out.String())
	}
}

// TestReplayCLITelemetryZeroHits points the OTLP exporter at a counting server
// and proves the replay path never exports a span.
func TestReplayCLITelemetryZeroHits(t *testing.T) {
	prepareReplayEnv(t)
	clearOTelEnv(t)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(telemetry.EnvEndpoint, server.URL)
	t.Setenv(telemetry.EnvProtocol, "http/protobuf")
	t.Setenv(telemetry.EnvServiceName, "recon-replay-test")

	fixture := newCLIReplayFixture(t)
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", cliReplayRunID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: replayLoader(t, fixture.Recording, &loads)})
	if err != nil {
		t.Fatalf("recon replay --exact: %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("OTLP endpoint hits = %d, want 0", got)
	}
}

// TestReplayCLIHelp pins that help is handled before config or flag errors.
func TestReplayCLIHelp(t *testing.T) {
	prepareReplayEnv(t)
	var out bytes.Buffer
	if err := runReplayTo(context.Background(), []string{"--help"}, &out, replayDeps{}); err != nil {
		t.Fatalf("recon replay --help: %v", err)
	}
	for _, want := range []string{
		"usage: recon replay",
		"--exact",
		"--rerun",
		"--live",
		"OPENCODE_API_KEY",
		"Exit status:",
		"2  usage/config/load/DB/cassette-miss/live-key error",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output does not contain %q:\n%s", want, out.String())
		}
	}
}
