package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// Outcome is one value of the agent_runs.outcome enum. The database CHECK
// constraint accepts exactly these five values.
type Outcome string

const (
	OutcomeProposal        Outcome = "proposal"
	OutcomeEscalated       Outcome = "escalated"
	OutcomeBudgetExhausted Outcome = "budget_exhausted"
	OutcomeInvalidOutput   Outcome = "invalid_output"
	OutcomeProviderError   Outcome = "provider_error"
)

// Valid reports whether o is one of the five terminal outcomes.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeProposal, OutcomeEscalated, OutcomeBudgetExhausted,
		OutcomeInvalidOutput, OutcomeProviderError:
		return true
	default:
		return false
	}
}

// RunRecord is the initial agent_runs row.
type RunRecord struct {
	ID            string
	CaseID        string
	Model         string
	PromptVersion string
	StartedAt     time.Time
}

// LLMCall is one successful model completion. Response and Usage are the
// normalized llm.Response and llm.Usage JSON.
type LLMCall struct {
	RunID       string
	Seq         int
	Model       string
	RequestHash string
	Response    json.RawMessage
	Usage       json.RawMessage
	StartedAt   time.Time
	DurationMS  int
}

// RunFinish is the single final agent_runs update.
type RunFinish struct {
	RunID     string
	EndedAt   time.Time
	TokensIn  int64
	TokensOut int64
	CostUSD   decimal.Decimal
	Outcome   Outcome
	Flags     []string
	Model     string
}

// ProposalRef is the proposal metadata a completed run reports.
type ProposalRef struct {
	ID     string
	Type   string
	Status string
}

// RunStore is the app-pool persistence surface used by the loop. No method
// opens a transaction across a model call.
type RunStore interface {
	CreateRun(ctx context.Context, run RunRecord) error
	RecordLLMCall(ctx context.Context, call LLMCall) error
	FinishRun(ctx context.Context, finish RunFinish) error
	ProposalForRun(ctx context.Context, runID string) (ProposalRef, error)
}
