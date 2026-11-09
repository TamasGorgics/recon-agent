package runner

import (
	"encoding/json"
	"regexp"
	"sort"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agentrun"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// Transcript snapshot v1 is the durable, bounded, sanitized view of one scored
// case. It is stored in eval_case_results.detail["transcript"] so reports can
// render a failure appendix after the sandbox clone is destroyed.
const (
	transcriptVersion           = 1
	transcriptMaxSteps          = 20
	transcriptMaxRefsPerStep    = 20
	transcriptMaxAssistantRunes = 200
	transcriptMaxErrorRunes     = 64
	transcriptMaxBytes          = 16 * 1024
	transcriptRedacted          = "[REDACTED]"
)

var (
	postgresDSNPattern = regexp.MustCompile(`(?i)\bpostgres(?:ql)?://[^\s"'<>]+`)
	passwordPattern    = regexp.MustCompile(`(?i)\bpassword(["']?\s*[:=]\s*)(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|\S+)`)
)

// transcriptSnapshotDoc is the versioned wire shape. It never carries raw tool
// arguments, results, memo text, DSNs, API keys, or session identifiers.
type transcriptSnapshotDoc struct {
	Version   int                    `json:"version"`
	Outcome   string                 `json:"outcome"`
	Proposal  *transcriptProposalDoc `json:"proposal"`
	Assistant string                 `json:"assistant"`
	ToolSteps []transcriptStepDoc    `json:"tool_steps"`
	Truncated bool                   `json:"truncated"`
}

type transcriptProposalDoc struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type transcriptStepDoc struct {
	Seq       int      `json:"seq"`
	Tool      string   `json:"tool"`
	Refs      []string `json:"refs"`
	ErrorCode string   `json:"error_code"`
	Truncated bool     `json:"truncated"`
}

// transcriptSnapshot builds the sanitized, deterministic, size-bounded
// snapshot for one finalizing case. The assistant is the already-sanitized
// agentrun.Report excerpt; this helper adds secret redaction and the v1 shape.
func transcriptSnapshot(report agentrun.Report, outcome, proposalID, proposalType string) map[string]any {
	doc := transcriptSnapshotDoc{
		Version:   transcriptVersion,
		Outcome:   outcome,
		Assistant: redactTranscriptText(report.Assistant),
		ToolSteps: make([]transcriptStepDoc, 0, len(report.ToolSteps)),
	}
	if proposalID != "" {
		doc.Proposal = &transcriptProposalDoc{ID: proposalID, Type: proposalType}
	}

	steps := append([]agentrun.Step(nil), report.ToolSteps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Seq < steps[j].Seq })
	truncated := false
	if len(steps) > transcriptMaxSteps {
		steps = steps[:transcriptMaxSteps]
		truncated = true
	}
	for _, step := range steps {
		refs := tools.SortedUnique(step.Refs)
		if len(refs) > transcriptMaxRefsPerStep {
			refs = refs[:transcriptMaxRefsPerStep]
			truncated = true
		}
		errorCode := step.ErrorCode
		if runes := []rune(errorCode); len(runes) > transcriptMaxErrorRunes {
			errorCode = string(runes[:transcriptMaxErrorRunes])
			truncated = true
		}
		doc.ToolSteps = append(doc.ToolSteps, transcriptStepDoc{
			Seq:       step.Seq,
			Tool:      step.Tool,
			Refs:      refs,
			ErrorCode: errorCode,
			Truncated: step.Truncated,
		})
	}
	if runes := []rune(doc.Assistant); len(runes) > transcriptMaxAssistantRunes {
		doc.Assistant = string(runes[:transcriptMaxAssistantRunes])
		truncated = true
	}
	doc.Truncated = truncated

	enforceTranscriptByteBound(&doc)
	return doc.toMap()
}

// enforceTranscriptByteBound drops refs first, then shortens the assistant,
// then drops trailing steps as a final defensive fallback. Snapshot-level
// truncated records that any bound was applied.
func enforceTranscriptByteBound(doc *transcriptSnapshotDoc) {
	if transcriptJSONSize(*doc) <= transcriptMaxBytes {
		return
	}
	doc.Truncated = true
	for index := range doc.ToolSteps {
		doc.ToolSteps[index].Refs = []string{}
	}
	if transcriptJSONSize(*doc) <= transcriptMaxBytes {
		return
	}

	runes := []rune(doc.Assistant)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		doc.Assistant = string(runes[:mid])
		if transcriptJSONSize(*doc) <= transcriptMaxBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	doc.Assistant = string(runes[:low])
	for len(doc.ToolSteps) > 0 && transcriptJSONSize(*doc) > transcriptMaxBytes {
		doc.ToolSteps = doc.ToolSteps[:len(doc.ToolSteps)-1]
	}
}

func transcriptJSONSize(doc transcriptSnapshotDoc) int {
	encoded, err := json.Marshal(doc)
	if err != nil {
		return transcriptMaxBytes + 1
	}
	return len(encoded)
}

func (doc transcriptSnapshotDoc) toMap() map[string]any {
	var proposal any
	if doc.Proposal != nil {
		proposal = doc.Proposal
	}
	return map[string]any{
		"version":    doc.Version,
		"outcome":    doc.Outcome,
		"proposal":   proposal,
		"assistant":  doc.Assistant,
		"tool_steps": doc.ToolSteps,
		"truncated":  doc.Truncated,
	}
}

// redactTranscriptText applies the shared LLM redaction classes and adds
// postgres/postgresql DSNs plus password key/value forms.
func redactTranscriptText(value string) string {
	value = llm.Redact(value)
	value = postgresDSNPattern.ReplaceAllString(value, transcriptRedacted)
	value = passwordPattern.ReplaceAllString(value, "password${1}"+transcriptRedacted)
	return value
}
