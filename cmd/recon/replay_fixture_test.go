package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/internal/replay/replaytest"
)

// committedRecordingPath is the checked-in recording the CLI exact path is
// pinned against. The loader is test-only; production never ingests JSON.
func committedRecordingPath() string {
	return filepath.Join("..", "..", "internal", "replay", "testdata", "recording_v1.json")
}

func loadCommittedRecording(t *testing.T) replay.Recording {
	t.Helper()
	rec, err := replaytest.LoadRecording(committedRecordingPath())
	if err != nil {
		t.Fatalf("load committed recording: %v", err)
	}
	return rec
}

// fixtureLoader injects the committed recording through the production load
// seam and asserts the CLI asked for exactly that run once.
func fixtureLoader(t *testing.T, rec replay.Recording, calls *int) func(context.Context, config.Config, string) (replay.Recording, error) {
	t.Helper()
	return func(_ context.Context, _ config.Config, runID string) (replay.Recording, error) {
		*calls++
		if runID != rec.Run.ID {
			t.Fatalf("load run id = %q, want %q", runID, rec.Run.ID)
		}
		return rec, nil
	}
}

// TestReplayCLIExactFromCommittedRecording is the committed-fixture exact path:
// one load, exit 0, a complete JSON report with the required keys.
func TestReplayCLIExactFromCommittedRecording(t *testing.T) {
	prepareReplayEnv(t)
	rec := loadCommittedRecording(t)
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", rec.Run.ID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: fixtureLoader(t, rec, &loads)})
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
	report := decodeCLIReplayReport(t, out.Bytes())
	if !report.Identical || report.Mode != replay.ModeExact || report.RunID != rec.Run.ID {
		t.Fatalf("report = %+v, want the identical committed run", report)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
	if len(report.Divergences) != 0 || len(report.Diagnostics) != 0 {
		t.Fatalf("divergences = %+v, diagnostics = %+v, want none", report.Divergences, report.Diagnostics)
	}
	if report.StepsTotal != 2 {
		t.Fatalf("steps_total = %d, want 2", report.StepsTotal)
	}
}

// TestReplayCLIExactCommittedDivergences mutates the committed fixture in
// memory: a request hash change exits 1 with request_hash_mismatch, and a
// stored proposal change exits 1 with proposal_digest_mismatch. Both must
// still print the full report on stdout.
func TestReplayCLIExactCommittedDivergences(t *testing.T) {
	t.Run("request hash", func(t *testing.T) {
		prepareReplayEnv(t)
		rec := loadCommittedRecording(t)
		rec.LLMCalls[1].RequestHash = strings.Repeat("a", 64)
		loads := 0
		var out bytes.Buffer
		err := runReplayTo(context.Background(), []string{
			"--run", rec.Run.ID, "--exact", "--json",
		}, &out, replayDeps{loadRecording: fixtureLoader(t, rec, &loads)})
		if err == nil || exitCode(err) != 1 {
			t.Fatalf("error = %v, want exit 1", err)
		}
		report := decodeCLIReplayReport(t, out.Bytes())
		if report.Identical {
			t.Fatal("identical = true for a request hash mutation")
		}
		if len(report.Divergences) != 1 ||
			report.Divergences[0].Code != replay.CodeRequestHashMismatch ||
			report.Divergences[0].Step != 2 {
			t.Fatalf("divergences = %+v, want request_hash_mismatch at step 2", report.Divergences)
		}
	})

	t.Run("proposal field", func(t *testing.T) {
		prepareReplayEnv(t)
		rec := loadCommittedRecording(t)
		rec.Proposal.Payload = []byte(`{"question":"A different stored question"}`)
		loads := 0
		var out bytes.Buffer
		err := runReplayTo(context.Background(), []string{
			"--run", rec.Run.ID, "--exact", "--json",
		}, &out, replayDeps{loadRecording: fixtureLoader(t, rec, &loads)})
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
}

// TestReplayCLIExactCommittedLegacyHash proves the legacy-NULL diagnostic is
// reported as metadata while the run still exits 0.
func TestReplayCLIExactCommittedLegacyHash(t *testing.T) {
	prepareReplayEnv(t)
	rec := loadCommittedRecording(t)
	for i := range rec.ToolCalls {
		rec.ToolCalls[i].ResultCanonicalHash = nil
	}
	loads := 0
	var out bytes.Buffer
	err := runReplayTo(context.Background(), []string{
		"--run", rec.Run.ID, "--exact", "--json",
	}, &out, replayDeps{loadRecording: fixtureLoader(t, rec, &loads)})
	if err != nil {
		t.Fatalf("recon replay --exact: %v", err)
	}
	if loads != 1 {
		t.Fatalf("load calls = %d, want exactly 1", loads)
	}
	report := decodeCLIReplayReport(t, out.Bytes())
	if !report.Identical {
		t.Fatalf("identical = false for legacy NULL hashes: %+v", report.Divergences)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
	if len(report.Diagnostics) != len(rec.ToolCalls) {
		t.Fatalf("diagnostics = %+v, want %d", report.Diagnostics, len(rec.ToolCalls))
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code != replay.CodeToolResultHashUnavailable {
			t.Fatalf("diagnostic = %+v, want %s", diagnostic, replay.CodeToolResultHashUnavailable)
		}
	}
}
