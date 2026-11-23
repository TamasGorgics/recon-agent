package replay_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/replay"
)

func TestExactReplayIdentical(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false, divergences = %+v, diagnostics = %+v", report.Divergences, report.Diagnostics)
	}
	if report.Mode != replay.ModeExact {
		t.Fatalf("mode = %q, want %q", report.Mode, replay.ModeExact)
	}
	if report.ProposalHash == "" || report.ProposalHash != report.StoredHash {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
	if len(report.Divergences) != 0 || len(report.Diagnostics) != 0 {
		t.Fatalf("divergences = %+v, diagnostics = %+v, want none", report.Divergences, report.Diagnostics)
	}
	if report.StateHash != testStateHash {
		t.Fatalf("state_hash = %q, want %q", report.StateHash, testStateHash)
	}
}

// covers: FIN-43
// TestExactReplayFakeClock proves replay timestamps come from the recording and
// are stable across two runs (no wall-clock read).
func TestExactReplayFakeClock(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	first, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	want := []replay.ClockEvent{
		{Kind: "tool", Step: 0, Tool: 1, At: testSeedTime, DurationMS: 0},
		{Kind: "llm", Step: 1, At: testLLMTime, DurationMS: 5},
		{Kind: "tool", Step: 1, Tool: 2, At: testProposalTime, DurationMS: 0},
	}
	if !reflect.DeepEqual(first.Clock, want) {
		t.Fatalf("clock events = %+v, want %+v", first.Clock, want)
	}
	time.Sleep(time.Millisecond)
	second, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay() second run: %v", err)
	}
	if !reflect.DeepEqual(first.Clock, second.Clock) {
		t.Fatalf("clock events are not stable: %+v vs %+v", first.Clock, second.Clock)
	}
}

// TestExactReplayRequestHashMismatch proves a mutated request hash is a step 1
// hard stop with no downstream proposal divergence.
func TestExactReplayRequestHashMismatch(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.LLMCalls[0].RequestHash = strings.Repeat("a", 64)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if report.Identical {
		t.Fatal("identical = true for a request hash mismatch")
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one", report.Divergences)
	}
	divergence := report.Divergences[0]
	if divergence.Code != replay.CodeRequestHashMismatch || divergence.Step != 1 {
		t.Fatalf("divergence = %+v, want request_hash_mismatch at step 1", divergence)
	}
	if report.ProposalHash != "" || report.StoredHash == "" {
		t.Fatalf("proposal_hash = %q, stored_hash = %q", report.ProposalHash, report.StoredHash)
	}
}

// TestExactReplayToolResultMismatch proves the recomputed canonical tool-result
// hash is compared with result_canonical_hash and a mismatch is a hard stop.
func TestExactReplayToolResultMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seq  int
		step int
	}{
		{name: "seed", seq: 1, step: 0},
		{name: "model tool call", seq: 2, step: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rec := syntheticRecording(t)
			bad := strings.Repeat("b", 64)
			for i := range rec.ToolCalls {
				if rec.ToolCalls[i].Seq == test.seq {
					rec.ToolCalls[i].ResultCanonicalHash = &bad
				}
			}
			report, err := replay.ExactReplay(rec, syntheticConfig())
			if err != nil {
				t.Fatalf("ExactReplay(): %v", err)
			}
			if report.Identical {
				t.Fatal("identical = true for a tool result mismatch")
			}
			if len(report.Divergences) != 1 {
				t.Fatalf("divergences = %+v, want one hard stop", report.Divergences)
			}
			divergence := report.Divergences[0]
			if divergence.Code != replay.CodeToolResultMismatch || divergence.Step != test.step {
				t.Fatalf("divergence = %+v, want tool_result_mismatch at step %d", divergence, test.step)
			}
		})
	}
}

// TestExactReplayMutatedResultBytes proves content mutation is caught even when
// the stored hash itself is left intact.
func TestExactReplayMutatedResultBytes(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.ToolCalls[1].Result = []byte(`{"data":{"status":"tampered"},"refs":[],"truncated":false,"untrusted_fields":[]}`)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if report.Identical || len(report.Divergences) != 1 ||
		report.Divergences[0].Code != replay.CodeToolResultMismatch {
		t.Fatalf("report = %+v, want one tool_result_mismatch", report)
	}
}

// TestExactReplayLegacyNullHashDiagnostic proves a NULL result_canonical_hash is
// diagnostic-only: it neither enters divergences nor changes identical.
func TestExactReplayLegacyNullHashDiagnostic(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	for i := range rec.ToolCalls {
		rec.ToolCalls[i].ResultCanonicalHash = nil
	}
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false for legacy NULL hashes, divergences = %+v", report.Divergences)
	}
	if len(report.Divergences) != 0 {
		t.Fatalf("divergences = %+v, want none", report.Divergences)
	}
	if len(report.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v, want two", report.Diagnostics)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code != replay.CodeToolResultHashUnavailable {
			t.Fatalf("diagnostic = %+v, want tool_result_hash_unavailable", diagnostic)
		}
	}
}

// TestExactReplayProposalDigestMismatch proves a changed stored proposal field
// changes the stored digest and is identity-breaking.
func TestExactReplayProposalDigestMismatch(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal.Payload = mustJSON(t, map[string]any{"question": "a different question"})
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if report.Identical {
		t.Fatal("identical = true for a proposal digest mismatch")
	}
	if report.ProposalHash == report.StoredHash {
		t.Fatalf("proposal_hash and stored_hash are equal: %s", report.ProposalHash)
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one digest mismatch", report.Divergences)
	}
	if report.Divergences[0].Code != replay.CodeProposalDigestMismatch ||
		report.Divergences[0].Step != 0 {
		t.Fatalf("divergence = %+v, want proposal_digest_mismatch at step 0", report.Divergences[0])
	}
}

// TestExactReplayProposalDigestBeforeKeyDiff proves the divergence order puts a
// digest mismatch before a key diff at the same step.
func TestExactReplayProposalDigestBeforeKeyDiff(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal.Payload = mustJSON(t, map[string]any{"question": "a different question"})
	rec.Proposal.IdempotencyKey = strings.Repeat("c", 64)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if len(report.Divergences) != 2 {
		t.Fatalf("divergences = %+v, want digest mismatch and key diff", report.Divergences)
	}
	if report.Divergences[0].Code != replay.CodeProposalDigestMismatch {
		t.Fatalf("first divergence = %+v, want proposal_digest_mismatch", report.Divergences[0])
	}
	if report.Divergences[1].Code != replay.CodeProposalFieldDiff ||
		report.Divergences[1].Field == nil || *report.Divergences[1].Field != "idempotency_key" {
		t.Fatalf("second divergence = %+v, want idempotency_key field diff", report.Divergences[1])
	}
}

// TestExactReplayKeyOnlyDiff proves a key-only difference is reported but does
// not change identical.
func TestExactReplayKeyOnlyDiff(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal.IdempotencyKey = strings.Repeat("c", 64)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false for a key-only diff, divergences = %+v", report.Divergences)
	}
	if report.ProposalHash != report.StoredHash {
		t.Fatalf("digests differ: %s vs %s", report.ProposalHash, report.StoredHash)
	}
	if len(report.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one", report.Divergences)
	}
	diff := report.Divergences[0]
	if diff.Code != replay.CodeProposalFieldDiff || diff.Step != 0 ||
		diff.Field == nil || *diff.Field != "idempotency_key" {
		t.Fatalf("divergence = %+v, want proposal_field_diff(idempotency_key)", diff)
	}
}

// TestExactReplayStateHashDiagnosticOnly proves state_hash is reported, is
// excluded from the digest, and never changes identity.
func TestExactReplayStateHashDiagnosticOnly(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Proposal.StateHash = strings.Repeat("d", 64)
	report, err := replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical || report.StateHash != strings.Repeat("d", 64) {
		t.Fatalf("report = %+v, want identical with the new state_hash", report)
	}
	rec.Proposal.StateHash = "not-a-hash"
	report, err = replay.ExactReplay(rec, syntheticConfig())
	if err != nil {
		t.Fatalf("ExactReplay(): %v", err)
	}
	if !report.Identical {
		t.Fatalf("identical = false for a malformed state_hash, divergences = %+v", report.Divergences)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != replay.CodeStateHashInvalid {
		t.Fatalf("diagnostics = %+v, want state_hash_invalid", report.Diagnostics)
	}
}

// TestExactReplayUnsupportedOutcome proves exact mode never fabricates identity
// for an unsupported outcome or a missing proposal row.
func TestExactReplayUnsupportedOutcome(t *testing.T) {
	t.Parallel()
	rec := syntheticRecording(t)
	rec.Run.Outcome = "escalated"
	_, err := replay.ExactReplay(rec, syntheticConfig())
	if !errors.Is(err, replay.ErrUnsupportedRun) {
		t.Fatalf("error = %v, want ErrUnsupportedRun", err)
	}
	var unsupported *replay.UnsupportedRunError
	if !errors.As(err, &unsupported) || unsupported.Outcome != "escalated" {
		t.Fatalf("error = %v, want UnsupportedRunError with outcome escalated", err)
	}

	rec = syntheticRecording(t)
	rec.Proposal = nil
	_, err = replay.ExactReplay(rec, syntheticConfig())
	if !errors.Is(err, replay.ErrUnsupportedRun) {
		t.Fatalf("missing proposal error = %v, want ErrUnsupportedRun", err)
	}
}

func TestLoadRecordingRejectsNonUUID(t *testing.T) {
	t.Parallel()
	_, err := replay.LoadRecording(context.Background(), nil, "not-a-uuid")
	if !errors.Is(err, replay.ErrInvalidRunID) {
		t.Fatalf("error = %v, want ErrInvalidRunID", err)
	}
}
