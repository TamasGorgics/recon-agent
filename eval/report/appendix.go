package report

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

const (
	transcriptStatusAvailable   = "available"
	transcriptStatusUnavailable = "unavailable"
	transcriptStatusUnsupported = "unsupported"

	transcriptVersion = 1
	transcriptRuneCap = 200
	transcriptRefCap  = 20
)

var (
	transcriptDSNPattern        = regexp.MustCompile(`(?i)\bpostgres(?:ql)?://[^\s"'<>]+`)
	transcriptPasswordPattern   = regexp.MustCompile(`(?i)\bpassword(["']?\s*[:=]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|\S+)`)
	transcriptControlCharacters = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	// transcriptRefSecretPattern covers credential shapes that the shared LLM
	// redactor does not know about but that must never survive in an evidence
	// ref: URI userinfo, token/api-key assignments, and live Stripe keys.
	transcriptRefSecretPattern = regexp.MustCompile(`(?i)(?:` +
		`\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@` +
		`|\b(?:token|api[_-]?key)\s*[:=]\s*\S+` +
		`|sk_live_[a-z0-9]+` +
		`)`)
)

// TranscriptSnapshot is the parsed, whitelisted version-1 transcript snapshot.
// Unknown keys in detail["transcript"] (raw args, results, memos, secrets) are
// never copied into the report model.
type TranscriptSnapshot struct {
	Version   int                 `json:"version"`
	Outcome   string              `json:"outcome"`
	Proposal  *TranscriptProposal `json:"proposal"`
	Assistant string              `json:"assistant"`
	ToolSteps []TranscriptStep    `json:"tool_steps"`
	Truncated bool                `json:"truncated"`
}

// TranscriptProposal is the safe proposal summary in a transcript snapshot.
type TranscriptProposal struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// TranscriptStep is one safe tool step. It never carries arguments or results.
type TranscriptStep struct {
	Seq       int      `json:"seq"`
	Tool      string   `json:"tool"`
	Refs      []string `json:"refs"`
	ErrorCode string   `json:"error_code"`
	Truncated bool     `json:"truncated"`
}

// AppendixCase is one deduplicated failure in the failure appendix. A repeat
// group lists a scenario once and counts repeats_failed out of repeats.
type AppendixCase struct {
	ScenarioID          string              `json:"scenario_id"`
	Mode                string              `json:"mode"`
	Difficulty          string              `json:"difficulty"`
	Outcome             string              `json:"outcome"`
	ExpectedResolutions []string            `json:"expected_resolutions"`
	ActualResolution    string              `json:"actual_resolution"`
	Correct             bool                `json:"correct"`
	FalseFix            bool                `json:"false_fix"`
	Escalated           bool                `json:"escalated"`
	Confidence          *float64            `json:"confidence"`
	EvidenceTotal       int                 `json:"evidence_total"`
	EvidenceReal        int                 `json:"evidence_real"`
	EvidenceRelevant    int                 `json:"evidence_relevant"`
	ErrorCode           string              `json:"error_code"`
	AgentRunID          string              `json:"agent_run_id"`
	ProposalID          string              `json:"proposal_id"`
	RepeatsFailed       int                 `json:"repeats_failed"`
	Repeats             int                 `json:"repeats"`
	Locator             string              `json:"locator"`
	TranscriptStatus    string              `json:"transcript_status"`
	Transcript          *TranscriptSnapshot `json:"transcript"`
}

type appendixEntry struct {
	category int
	example  score.CaseResult
	run      RunRow
	failed   int
	repeats  int
}

func buildAppendix(runData []runReport, truth map[string]dataset.ScenarioTruth,
	requested RunRow, opts Options) ([]AppendixCase, []string) {
	entries := make(map[string]*appendixEntry)
	for _, data := range runData {
		for _, result := range data.results {
			if !result.FalseFix && result.Correct {
				continue
			}
			entry, exists := entries[result.ScenarioID]
			if !exists {
				entry = &appendixEntry{
					category: failureCategory(result),
					example:  result,
					run:      data.run,
					repeats:  len(runData),
				}
				entries[result.ScenarioID] = entry
			}
			entry.failed++
		}
	}
	ordered := make([]*appendixEntry, 0, len(entries))
	for _, entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.category != right.category {
			return left.category < right.category
		}
		if left.example.Mode != right.example.Mode {
			return left.example.Mode < right.example.Mode
		}
		if left.example.ScenarioID != right.example.ScenarioID {
			return left.example.ScenarioID < right.example.ScenarioID
		}
		return repeatIndexValue(left.run) < repeatIndexValue(right.run)
	})

	limit := opts.AppendixLimit
	if limit > len(ordered) {
		limit = len(ordered)
	}
	cases := make([]AppendixCase, 0, limit)
	warnings := make([]string, 0)
	for _, entry := range ordered[:limit] {
		scenario := truth[entry.example.ScenarioID]
		detail := entry.example.Detail
		transcript, status, warning := parseTranscript(detail)
		if warning != "" {
			warnings = append(warnings, fmt.Sprintf("scenario %s: %s",
				entry.example.ScenarioID, warning))
		}
		agentRunID := redactTranscriptText(detailString(detail, "agent_run_id"))
		cases = append(cases, AppendixCase{
			ScenarioID:          redactTranscriptText(entry.example.ScenarioID),
			Mode:                redactTranscriptText(entry.example.Mode),
			Difficulty:          redactTranscriptText(scenario.Difficulty),
			Outcome:             redactTranscriptText(detailString(detail, "outcome")),
			ExpectedResolutions: redactTranscriptValues(scenario.ExpectedResolutions),
			ActualResolution:    redactTranscriptText(detailString(detail, score.DetailActualResolution)),
			Correct:             entry.example.Correct,
			FalseFix:            entry.example.FalseFix,
			Escalated:           entry.example.Escalated,
			Confidence:          decimalFloat(entry.example.Confidence),
			EvidenceTotal:       detailInt(detail, score.DetailEvidenceTotal),
			EvidenceReal:        detailInt(detail, score.DetailEvidenceReal),
			EvidenceRelevant:    detailInt(detail, score.DetailEvidenceRelevant),
			ErrorCode:           redactTranscriptText(detailString(detail, score.DetailError)),
			AgentRunID:          agentRunID,
			ProposalID:          redactTranscriptText(detailString(detail, "proposal_id")),
			RepeatsFailed:       entry.failed,
			Repeats:             entry.repeats,
			Locator: transcriptLocator(entry.run.ID,
				redactTranscriptText(entry.example.ScenarioID), agentRunID),
			TranscriptStatus: status,
			Transcript:       transcript,
		})
	}
	return cases, warnings
}

func failureCategory(result score.CaseResult) int {
	if result.FalseFix {
		return 0
	}
	outcome := detailString(result.Detail, "outcome")
	switch outcome {
	case "provider_error", "budget_exhausted", "invalid_output":
		return 3
	}
	if result.Escalated {
		return 2
	}
	return 1
}

func transcriptLocator(runID, scenarioID, agentRunID string) string {
	if agentRunID == "" {
		return fmt.Sprintf("run:%s scenario:%s", runID, scenarioID)
	}
	return fmt.Sprintf("run:%s scenario:%s agent_run:%s", runID, scenarioID, agentRunID)
}

func parseTranscript(detail map[string]any) (*TranscriptSnapshot, string, string) {
	raw, ok := detail["transcript"]
	if !ok || raw == nil {
		return nil, transcriptStatusUnavailable, ""
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, transcriptStatusUnavailable, "transcript marker is not JSON"
	}
	var header struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(encoded, &header); err != nil {
		return nil, transcriptStatusUnavailable, "transcript marker is malformed"
	}
	if header.Version == nil {
		return nil, transcriptStatusUnavailable, "transcript marker has no version"
	}
	if *header.Version != transcriptVersion {
		return nil, transcriptStatusUnsupported, fmt.Sprintf(
			"transcript version %d is unsupported", *header.Version)
	}
	var snapshot TranscriptSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, transcriptStatusUnavailable, "transcript v1 is malformed"
	}
	sanitizeTranscriptSnapshot(&snapshot)
	// The typed snapshot is a whitelist: any raw args/results/memos/secrets
	// present in an untrusted marker were dropped by the decode above. The
	// strings that remain are re-redacted because a foreign v1 row may predate
	// or bypass the writer-side sanitizer.
	return &snapshot, transcriptStatusAvailable, ""
}

// sanitizeTranscriptSnapshot re-applies full redaction to every string field a
// foreign v1 snapshot can place in the report. Refs are additionally filtered
// to canonical, secret-free evidence references.
func sanitizeTranscriptSnapshot(snapshot *TranscriptSnapshot) {
	snapshot.Outcome = redactTranscriptText(snapshot.Outcome)
	snapshot.Assistant = sanitizeAssistant(snapshot.Assistant)
	if snapshot.Proposal != nil {
		snapshot.Proposal.ID = redactTranscriptText(snapshot.Proposal.ID)
		snapshot.Proposal.Type = redactTranscriptText(snapshot.Proposal.Type)
	}
	if snapshot.ToolSteps == nil {
		snapshot.ToolSteps = []TranscriptStep{}
		return
	}
	for index := range snapshot.ToolSteps {
		step := &snapshot.ToolSteps[index]
		step.Tool = redactTranscriptText(step.Tool)
		step.Refs = sanitizeTranscriptRefs(step.Refs)
		step.ErrorCode = redactTranscriptText(step.ErrorCode)
	}
}

func sanitizeAssistant(text string) string {
	text = redactTranscriptText(text)
	runes := []rune(text)
	if len(runes) > transcriptRuneCap {
		text = string(runes[:transcriptRuneCap])
	}
	return text
}

func redactTranscriptText(text string) string {
	text = llm.Redact(text)
	text = transcriptDSNPattern.ReplaceAllString(text, "[REDACTED]")
	text = transcriptPasswordPattern.ReplaceAllString(text, "password${1}[REDACTED]")
	return text
}

func redactTranscriptValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, redactTranscriptText(value))
	}
	return out
}

// transcriptRefCarriesSecret reports whether a ref contains any known
// credential class, including classes the generic text redactor handles plus
// URI userinfo, token/api_key assignments, and live Stripe keys.
func transcriptRefCarriesSecret(ref string) bool {
	if redactTranscriptText(ref) != ref {
		return true
	}
	return transcriptRefSecretPattern.MatchString(ref)
}

// sanitizeTranscriptRefs keeps only canonical evidence refs that carry no
// secret material. A ref needing redaction, containing control characters, or
// failing tools.ValidateRef is dropped whole: a partially redacted ext:
// reference no longer identifies a real record and must not look like evidence.
func sanitizeTranscriptRefs(refs []string) []string {
	if len(refs) == 0 {
		return []string{}
	}
	safe := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref == "" || transcriptControlCharacters.MatchString(ref) {
			continue
		}
		if transcriptRefCarriesSecret(ref) {
			continue
		}
		if err := tools.ValidateRef(ref); err != nil {
			continue
		}
		safe = append(safe, ref)
	}
	safe = tools.SortedUnique(safe)
	if len(safe) > transcriptRefCap {
		safe = safe[:transcriptRefCap]
	}
	return safe
}

func decimalFloat(value *decimal.Decimal) *float64 {
	if value == nil {
		return nil
	}
	converted := value.InexactFloat64()
	return &converted
}

func detailString(detail map[string]any, key string) string {
	if detail == nil {
		return ""
	}
	value, _ := detail[key].(string)
	return value
}

func detailInt(detail map[string]any, key string) int {
	if detail == nil {
		return 0
	}
	switch value := detail[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}

func joinOrNA(values []string) string {
	if len(values) == 0 {
		return "n/a"
	}
	return strings.Join(values, ", ")
}
