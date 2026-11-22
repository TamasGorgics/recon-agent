package replay_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/replay"
	"github.com/TamasGorgics/recon-agent/internal/replay/replaytest"
)

const recordingFixturePath = "testdata/recording_v1.json"

func loadRecordingFixture(t *testing.T) replay.Recording {
	t.Helper()
	rec, err := replaytest.LoadRecording(recordingFixturePath)
	if err != nil {
		t.Fatalf("load %s: %v", recordingFixturePath, err)
	}
	return rec
}

// covers: FIN-43
// TestRecordingFixtureExactIdentical is the committed-fixture round trip: the
// checked-in recording must replay identically through the same engine the CLI
// uses, including the request hashes and the canonical tool-result hashes.
func TestRecordingFixtureExactIdentical(t *testing.T) {
	t.Parallel()
	rec := loadRecordingFixture(t)
	if len(rec.LLMCalls) != 2 || len(rec.ToolCalls) != 3 {
		t.Fatalf("fixture llm_calls = %d, tool_calls = %d, want 2/3", len(rec.LLMCalls), len(rec.ToolCalls))
	}
	if rec.Proposal == nil {
		t.Fatal("fixture has no stored proposal")
	}
	for _, call := range rec.ToolCalls {
		if call.ResultCanonicalHash == nil || len(*call.ResultCanonicalHash) != 64 {
			t.Fatalf("fixture tool_calls.seq %d has result_canonical_hash %v, want 64 hex", call.Seq, call.ResultCanonicalHash)
		}
	}
	for _, call := range rec.LLMCalls {
		if len(call.RequestHash) != 64 {
			t.Fatalf("fixture llm_calls.seq %d has request_hash %q, want 64 hex", call.Seq, call.RequestHash)
		}
	}

	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false: divergences=%+v diagnostics=%+v", report.Divergences, report.Diagnostics)
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
	// The fake clock is driven by the fixture's recorded timestamps.
	wantClock := []replay.ClockEvent{
		{Kind: "tool", Step: 0, Tool: 1, At: rec.ToolCalls[0].StartedAt, DurationMS: rec.ToolCalls[0].DurationMS},
		{Kind: "llm", Step: 1, At: rec.LLMCalls[0].StartedAt, DurationMS: rec.LLMCalls[0].DurationMS},
		{Kind: "tool", Step: 1, Tool: 2, At: rec.ToolCalls[1].StartedAt, DurationMS: rec.ToolCalls[1].DurationMS},
		{Kind: "llm", Step: 2, At: rec.LLMCalls[1].StartedAt, DurationMS: rec.LLMCalls[1].DurationMS},
		{Kind: "tool", Step: 2, Tool: 3, At: rec.ToolCalls[2].StartedAt, DurationMS: rec.ToolCalls[2].DurationMS},
	}
	if !reflect.DeepEqual(report.Clock, wantClock) {
		t.Fatalf("clock events = %+v, want %+v", report.Clock, wantClock)
	}
	second, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay() second run: %v", err)
	}
	if !reflect.DeepEqual(report.Clock, second.Clock) {
		t.Fatalf("clock events are not stable: %+v vs %+v", report.Clock, second.Clock)
	}
}

// TestRecordingFixtureRoundTrip proves the test-only writer/loader pair and the
// committed file agree field for field.
func TestRecordingFixtureRoundTrip(t *testing.T) {
	t.Parallel()
	rec := loadRecordingFixture(t)
	path := filepath.Join(t.TempDir(), "recording.json")
	if err := replaytest.WriteRecording(path, rec); err != nil {
		t.Fatalf("WriteRecording(): %v", err)
	}
	reloaded, err := replaytest.LoadRecording(path)
	if err != nil {
		t.Fatalf("LoadRecording() round trip: %v", err)
	}
	if !reflect.DeepEqual(rec, reloaded) {
		t.Fatalf("round trip changed the recording:\nfirst  = %+v\nsecond = %+v", rec, reloaded)
	}
}

// TestRecordingFixtureDivergences derives the committed divergence fixtures by
// mutation and asserts the exact codes the CLI maps to exit 1.
func TestRecordingFixtureDivergences(t *testing.T) {
	t.Parallel()
	t.Run("request hash", func(t *testing.T) {
		t.Parallel()
		rec := loadRecordingFixture(t)
		rec.LLMCalls[1].RequestHash = strings.Repeat("a", 64)
		report, err := replay.ExactReplay(rec, syntheticConfig())
		if err != nil {
			t.Fatalf("ExactReplay(): %v", err)
		}
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
		t.Parallel()
		rec := loadRecordingFixture(t)
		rec.Proposal.Payload = []byte(`{"question":"A different stored question"}`)
		report, err := replay.ExactReplay(rec, syntheticConfig())
		if err != nil {
			t.Fatalf("ExactReplay(): %v", err)
		}
		if report.Identical {
			t.Fatal("identical = true for a stored proposal mutation")
		}
		if len(report.Divergences) != 1 ||
			report.Divergences[0].Code != replay.CodeProposalDigestMismatch ||
			report.Divergences[0].Step != 0 {
			t.Fatalf("divergences = %+v, want proposal_digest_mismatch at step 0", report.Divergences)
		}
	})

	t.Run("tool result", func(t *testing.T) {
		t.Parallel()
		rec := loadRecordingFixture(t)
		bad := strings.Repeat("b", 64)
		rec.ToolCalls[1].ResultCanonicalHash = &bad
		report, err := replay.ExactReplay(rec, syntheticConfig())
		if err != nil {
			t.Fatalf("ExactReplay(): %v", err)
		}
		if report.Identical {
			t.Fatal("identical = true for a tool result mutation")
		}
		if len(report.Divergences) != 1 ||
			report.Divergences[0].Code != replay.CodeToolResultMismatch ||
			report.Divergences[0].Step != 1 {
			t.Fatalf("divergences = %+v, want tool_result_mismatch at step 1", report.Divergences)
		}
	})

	t.Run("legacy null hash is diagnostic", func(t *testing.T) {
		t.Parallel()
		rec := loadRecordingFixture(t)
		for i := range rec.ToolCalls {
			rec.ToolCalls[i].ResultCanonicalHash = nil
		}
		report, err := replay.ExactReplay(rec, syntheticConfig())
		if err != nil {
			t.Fatalf("ExactReplay(): %v", err)
		}
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
	})
}
