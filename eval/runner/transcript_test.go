package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/agentrun"
)

func transcriptJSON(t *testing.T, snapshot map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal transcript snapshot: %v", err)
	}
	return string(encoded)
}

func transcriptSteps(t *testing.T, snapshot map[string]any) []transcriptStepDoc {
	t.Helper()
	steps, ok := snapshot["tool_steps"].([]transcriptStepDoc)
	if !ok {
		t.Fatalf("tool_steps has type %T, want []transcriptStepDoc", snapshot["tool_steps"])
	}
	return steps
}

func TestTranscriptSnapshotOrdersStepsAndKeepsSafeFields(t *testing.T) {
	report := agentrun.Report{
		Assistant: "I will link the matching ledger transaction.",
		ToolSteps: []agentrun.Step{
			{
				Seq: 3, Tool: "propose_link", ErrorCode: "invalid_arguments", Truncated: true,
				Refs: []string{"external_record:psp_a:rec-2", "case:123e4567-e89b-42d3-a456-426614174000", "external_record:psp_a:rec-2"},
			},
			{Seq: 1, Tool: "get_case", Refs: []string{"case:123e4567-e89b-42d3-a456-426614174000"}},
			{Seq: 2, Tool: "get_ledger", Refs: []string{"ledger_txn:223e4567-e89b-42d3-a456-426614174000"}},
		},
	}

	snapshot := transcriptSnapshot(report, "proposal",
		"323e4567-e89b-42d3-a456-426614174000", "link")

	if snapshot["version"] != transcriptVersion {
		t.Fatalf("version = %v, want %d", snapshot["version"], transcriptVersion)
	}
	if snapshot["outcome"] != "proposal" {
		t.Fatalf("outcome = %v", snapshot["outcome"])
	}
	if snapshot["truncated"] != false {
		t.Fatalf("truncated = %v, want false", snapshot["truncated"])
	}
	if snapshot["assistant"] != report.Assistant {
		t.Fatalf("assistant = %q, want %q", snapshot["assistant"], report.Assistant)
	}
	proposal, ok := snapshot["proposal"].(*transcriptProposalDoc)
	if !ok || proposal == nil {
		t.Fatalf("proposal = %#v, want summary", snapshot["proposal"])
	}
	if proposal.ID != "323e4567-e89b-42d3-a456-426614174000" || proposal.Type != "link" {
		t.Fatalf("proposal = %+v", proposal)
	}

	steps := transcriptSteps(t, snapshot)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for index, want := range []int{1, 2, 3} {
		if steps[index].Seq != want {
			t.Fatalf("step %d seq = %d, want %d", index, steps[index].Seq, want)
		}
	}
	last := steps[2]
	if last.Tool != "propose_link" || last.ErrorCode != "invalid_arguments" || !last.Truncated {
		t.Fatalf("last step = %+v", last)
	}
	if got, want := strings.Join(last.Refs, ","), "case:123e4567-e89b-42d3-a456-426614174000,external_record:psp_a:rec-2"; got != want {
		t.Fatalf("last step refs = %q, want sorted unique %q", got, want)
	}
}

func TestTranscriptSnapshotRedactsSecrets(t *testing.T) {
	assistant := `memo: customer referenced key oc_sk_synthetic_secret and Bearer abc.def-123; ` +
		`authorization: auth-secret; session ses_abc123; dsn postgres://user:pass@localhost:5432/recon; ` +
		`password=hunter2; postgresql://admin:letmein@db.example/recon`
	report := agentrun.Report{Assistant: assistant}

	snapshot := transcriptSnapshot(report, "escalated", "", "")
	text := transcriptJSON(t, snapshot)

	for _, secret := range []string{
		"oc_sk_synthetic_secret",
		"abc.def-123",
		"auth-secret",
		"ses_abc123",
		"postgres://user:pass@localhost:5432/recon",
		"hunter2",
		"postgresql://admin:letmein@db.example/recon",
		"letmein",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("transcript leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("transcript did not record redaction: %s", text)
	}
	if !strings.Contains(text, "memo:") {
		t.Fatalf("non-secret assistant context was dropped: %s", text)
	}
}

func TestTranscriptSnapshotTruncatesAssistantTo200Runes(t *testing.T) {
	assistant := strings.Repeat("é", 300)
	snapshot := transcriptSnapshot(agentrun.Report{Assistant: assistant}, "", "", "")

	got, ok := snapshot["assistant"].(string)
	if !ok {
		t.Fatalf("assistant has type %T", snapshot["assistant"])
	}
	if runes := utf8.RuneCountInString(got); runes != transcriptMaxAssistantRunes {
		t.Fatalf("assistant runes = %d, want %d", runes, transcriptMaxAssistantRunes)
	}
	if snapshot["truncated"] != true {
		t.Fatalf("truncated = %v, want true after assistant truncation", snapshot["truncated"])
	}
}

func TestTranscriptSnapshotBoundsLargeRefSet(t *testing.T) {
	steps := make([]agentrun.Step, 25)
	for index := range steps {
		refs := make([]string, 30)
		for refIndex := range refs {
			refs[refIndex] = fmt.Sprintf("external_record:psp_a:rec-%04d-%s", refIndex, strings.Repeat("x", 200))
		}
		steps[index] = agentrun.Step{Seq: index + 1, Tool: "get_external_records", Refs: refs}
	}
	assistant := "short assistant excerpt"
	report := agentrun.Report{Assistant: assistant, ToolSteps: steps}

	snapshot := transcriptSnapshot(report, "proposal", "323e4567-e89b-42d3-a456-426614174000", "link")
	text := transcriptJSON(t, snapshot)
	if len(text) > transcriptMaxBytes {
		t.Fatalf("transcript bytes = %d, want <= %d", len(text), transcriptMaxBytes)
	}
	if snapshot["truncated"] != true {
		t.Fatalf("truncated = %v, want true", snapshot["truncated"])
	}
	boundedSteps := transcriptSteps(t, snapshot)
	if len(boundedSteps) > transcriptMaxSteps {
		t.Fatalf("steps = %d, want <= %d", len(boundedSteps), transcriptMaxSteps)
	}
	for _, step := range boundedSteps {
		if len(step.Refs) > transcriptMaxRefsPerStep {
			t.Fatalf("step %d refs = %d, want <= %d", step.Seq, len(step.Refs), transcriptMaxRefsPerStep)
		}
	}
	// Refs are dropped before the assistant is shortened.
	if snapshot["assistant"] != assistant {
		t.Fatalf("assistant = %q, want %q after ref trimming", snapshot["assistant"], assistant)
	}
}

func TestTranscriptSnapshotShortensAssistantWhenRefsAreNotEnough(t *testing.T) {
	steps := make([]agentrun.Step, transcriptMaxSteps)
	for index := range steps {
		steps[index] = agentrun.Step{Seq: index + 1, Tool: strings.Repeat("t", 2000)}
	}
	assistant := strings.Repeat("a", transcriptMaxAssistantRunes)
	report := agentrun.Report{Assistant: assistant, ToolSteps: steps}

	snapshot := transcriptSnapshot(report, "proposal", "323e4567-e89b-42d3-a456-426614174000", "link")
	text := transcriptJSON(t, snapshot)
	if len(text) > transcriptMaxBytes {
		t.Fatalf("transcript bytes = %d, want <= %d", len(text), transcriptMaxBytes)
	}
	if snapshot["truncated"] != true {
		t.Fatalf("truncated = %v, want true", snapshot["truncated"])
	}
	if snapshot["assistant"] == assistant {
		t.Fatal("assistant was not shortened after the byte bound was exceeded")
	}
}

func TestTranscriptSnapshotEmptyReport(t *testing.T) {
	snapshot := transcriptSnapshot(agentrun.Report{}, "", "", "")

	if snapshot["version"] != transcriptVersion {
		t.Fatalf("version = %v, want %d", snapshot["version"], transcriptVersion)
	}
	if snapshot["outcome"] != "" {
		t.Fatalf("outcome = %v, want empty", snapshot["outcome"])
	}
	if snapshot["proposal"] != nil {
		t.Fatalf("proposal = %#v, want nil", snapshot["proposal"])
	}
	if snapshot["assistant"] != "" {
		t.Fatalf("assistant = %q, want empty", snapshot["assistant"])
	}
	if snapshot["truncated"] != false {
		t.Fatalf("truncated = %v, want false", snapshot["truncated"])
	}
	if steps := transcriptSteps(t, snapshot); steps == nil || len(steps) != 0 {
		t.Fatalf("steps = %#v, want empty non-nil", snapshot["tool_steps"])
	}
	text := transcriptJSON(t, snapshot)
	if !strings.Contains(text, `"tool_steps":[]`) {
		t.Fatalf("empty snapshot tool_steps is not an empty array: %s", text)
	}
}

func TestTranscriptSnapshotHasNoRawDataKeys(t *testing.T) {
	report := agentrun.Report{
		Assistant: "clean assistant excerpt",
		ToolSteps: []agentrun.Step{{
			Seq: 1, Tool: "get_case",
			Refs: []string{"case:123e4567-e89b-42d3-a456-426614174000"},
		}},
	}
	text := transcriptJSON(t, transcriptSnapshot(report, "proposal", "323e4567-e89b-42d3-a456-426614174000", "link"))
	for _, key := range []string{`"args"`, `"result"`, `"raw"`, `"memo"`, `"dsn"`, `"api_key"`} {
		if strings.Contains(text, key) {
			t.Fatalf("transcript contains raw-data key %s: %s", key, text)
		}
	}
}
