// Package replaytest provides test-only helpers for the committed replay
// fixtures under internal/replay/testdata. It is imported only from _test.go
// files: it is deliberately not a production JSON ingestion path, and no
// production package may depend on it.
package replaytest

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/replay"
)

// FileSchema is the recording fixture schema version.
const FileSchema = 1

// recordingFile is the committed, reviewable JSON shape of one recording.
// Raw JSON columns stay nested so the fixture reads like the stored rows.
type recordingFile struct {
	Schema    int            `json:"schema"`
	Run       runFile        `json:"run"`
	LLMCalls  []llmCallFile  `json:"llm_calls"`
	ToolCalls []toolCallFile `json:"tool_calls"`
	Proposal  *proposalFile  `json:"proposal"`
}

type runFile struct {
	ID            string  `json:"id"`
	CaseID        string  `json:"case_id"`
	Model         string  `json:"model"`
	PromptVersion string  `json:"prompt_version"`
	Outcome       string  `json:"outcome"`
	StartedAt     string  `json:"started_at"`
	EndedAt       *string `json:"ended_at"`
}

type llmCallFile struct {
	Seq         int             `json:"seq"`
	Model       string          `json:"model"`
	RequestHash string          `json:"request_hash"`
	Response    json.RawMessage `json:"response"`
	Usage       json.RawMessage `json:"usage"`
	StartedAt   string          `json:"started_at"`
	DurationMS  int             `json:"duration_ms"`
}

type toolCallFile struct {
	Seq                 int             `json:"seq"`
	ToolName            string          `json:"tool_name"`
	Args                json.RawMessage `json:"args"`
	Result              json.RawMessage `json:"result"`
	ResultHash          string          `json:"result_hash"`
	ResultCanonicalHash *string         `json:"result_canonical_hash"`
	Error               *string         `json:"error"`
	InjectionFlags      json.RawMessage `json:"injection_flags"`
	StartedAt           string          `json:"started_at"`
	DurationMS          int             `json:"duration_ms"`
}

type proposalFile struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Rationale      string          `json:"rationale"`
	Evidence       json.RawMessage `json:"evidence"`
	Confidence     string          `json:"confidence"`
	PatternClaimed *string         `json:"pattern_claimed"`
	StateHash      string          `json:"state_hash"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// LoadRecording reads one committed recording fixture. The conversion is
// mechanical: nested JSON columns become the raw column text replay already
// consumes from PostgreSQL, and timestamps are parsed strictly.
func LoadRecording(path string) (replay.Recording, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return replay.Recording{}, fmt.Errorf("replaytest: read recording fixture: %w", err)
	}
	var file recordingFile
	if err := json.Unmarshal(data, &file); err != nil {
		return replay.Recording{}, fmt.Errorf("replaytest: decode recording fixture: %w", err)
	}
	if file.Schema != FileSchema {
		return replay.Recording{}, fmt.Errorf("replaytest: recording fixture schema = %d, want %d", file.Schema, FileSchema)
	}
	if file.Run.ID == "" || file.Run.CaseID == "" {
		return replay.Recording{}, fmt.Errorf("replaytest: recording fixture is missing run id or case id")
	}

	startedAt, err := parseTime("run.started_at", file.Run.StartedAt)
	if err != nil {
		return replay.Recording{}, err
	}
	var endedAt *time.Time
	if file.Run.EndedAt != nil {
		parsed, err := parseTime("run.ended_at", *file.Run.EndedAt)
		if err != nil {
			return replay.Recording{}, err
		}
		endedAt = &parsed
	}

	rec := replay.Recording{
		Run: replay.Run{
			ID:            file.Run.ID,
			CaseID:        file.Run.CaseID,
			Model:         file.Run.Model,
			PromptVersion: file.Run.PromptVersion,
			Outcome:       file.Run.Outcome,
			StartedAt:     startedAt,
			EndedAt:       endedAt,
		},
		LLMCalls:  make([]replay.LLMCall, 0, len(file.LLMCalls)),
		ToolCalls: make([]replay.ToolCall, 0, len(file.ToolCalls)),
	}
	for _, call := range file.LLMCalls {
		callStarted, err := parseTime("llm_calls.started_at", call.StartedAt)
		if err != nil {
			return replay.Recording{}, err
		}
		rec.LLMCalls = append(rec.LLMCalls, replay.LLMCall{
			Seq:         call.Seq,
			Model:       call.Model,
			RequestHash: call.RequestHash,
			Response:    rawOrNull(call.Response),
			Usage:       rawOrNull(call.Usage),
			StartedAt:   callStarted,
			DurationMS:  call.DurationMS,
		})
	}
	for _, call := range file.ToolCalls {
		callStarted, err := parseTime("tool_calls.started_at", call.StartedAt)
		if err != nil {
			return replay.Recording{}, err
		}
		rec.ToolCalls = append(rec.ToolCalls, replay.ToolCall{
			Seq:                 call.Seq,
			ToolName:            call.ToolName,
			Args:                rawOrNull(call.Args),
			Result:              rawOrNull(call.Result),
			ResultHash:          call.ResultHash,
			ResultCanonicalHash: call.ResultCanonicalHash,
			Error:               call.Error,
			InjectionFlags:      rawOrEmptyArray(call.InjectionFlags),
			StartedAt:           callStarted,
			DurationMS:          call.DurationMS,
		})
	}
	if file.Proposal != nil {
		rec.Proposal = &replay.Proposal{
			ID:             file.Proposal.ID,
			Type:           file.Proposal.Type,
			Payload:        rawOrNull(file.Proposal.Payload),
			Rationale:      file.Proposal.Rationale,
			Evidence:       rawOrNull(file.Proposal.Evidence),
			Confidence:     file.Proposal.Confidence,
			PatternClaimed: file.Proposal.PatternClaimed,
			StateHash:      file.Proposal.StateHash,
			IdempotencyKey: file.Proposal.IdempotencyKey,
		}
	}
	return rec, nil
}

// WriteRecording writes one recording in the committed fixture schema. It
// exists so the checked-in fixture can be regenerated deterministically with
// real recording code; production never calls it.
func WriteRecording(path string, rec replay.Recording) error {
	file := recordingFile{
		Schema: FileSchema,
		Run: runFile{
			ID:            rec.Run.ID,
			CaseID:        rec.Run.CaseID,
			Model:         rec.Run.Model,
			PromptVersion: rec.Run.PromptVersion,
			Outcome:       rec.Run.Outcome,
			StartedAt:     rec.Run.StartedAt.UTC().Format(time.RFC3339Nano),
			EndedAt:       formatTimePtr(rec.Run.EndedAt),
		},
		LLMCalls:  make([]llmCallFile, 0, len(rec.LLMCalls)),
		ToolCalls: make([]toolCallFile, 0, len(rec.ToolCalls)),
	}
	for _, call := range rec.LLMCalls {
		file.LLMCalls = append(file.LLMCalls, llmCallFile{
			Seq:         call.Seq,
			Model:       call.Model,
			RequestHash: call.RequestHash,
			Response:    rawOrNull(call.Response),
			Usage:       rawOrNull(call.Usage),
			StartedAt:   call.StartedAt.UTC().Format(time.RFC3339Nano),
			DurationMS:  call.DurationMS,
		})
	}
	for _, call := range rec.ToolCalls {
		file.ToolCalls = append(file.ToolCalls, toolCallFile{
			Seq:                 call.Seq,
			ToolName:            call.ToolName,
			Args:                rawOrNull(call.Args),
			Result:              rawOrNull(call.Result),
			ResultHash:          call.ResultHash,
			ResultCanonicalHash: call.ResultCanonicalHash,
			Error:               call.Error,
			InjectionFlags:      rawOrEmptyArray(call.InjectionFlags),
			StartedAt:           call.StartedAt.UTC().Format(time.RFC3339Nano),
			DurationMS:          call.DurationMS,
		})
	}
	if rec.Proposal != nil {
		file.Proposal = &proposalFile{
			ID:             rec.Proposal.ID,
			Type:           rec.Proposal.Type,
			Payload:        rawOrNull(rec.Proposal.Payload),
			Rationale:      rec.Proposal.Rationale,
			Evidence:       rawOrNull(rec.Proposal.Evidence),
			Confidence:     rec.Proposal.Confidence,
			PatternClaimed: rec.Proposal.PatternClaimed,
			StateHash:      rec.Proposal.StateHash,
			IdempotencyKey: rec.Proposal.IdempotencyKey,
		}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("replaytest: encode recording fixture: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("replaytest: write recording fixture: %w", err)
	}
	return nil
}

func parseTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("replaytest: recording fixture field %s is empty", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("replaytest: recording fixture field %s: %w", field, err)
	}
	return parsed.UTC(), nil
}

func formatTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func rawOrNull(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("null")
	}
	return append([]byte(nil), value...)
}

func rawOrEmptyArray(value json.RawMessage) []byte {
	if len(value) == 0 {
		return []byte("[]")
	}
	return append([]byte(nil), value...)
}
