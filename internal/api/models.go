package api

import (
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// CaseSummary is one row of GET /v1/cases.
type CaseSummary struct {
	ID         string    `json:"id"`
	Priority   int       `json:"priority"`
	ValueMinor int64     `json:"value_minor"`
	Currency   string    `json:"currency"`
	Status     string    `json:"status"`
	Version    int       `json:"version"`
	Attempts   int       `json:"attempts"`
	CreatedAt  time.Time `json:"created_at"`
	BreakIDs   []string  `json:"break_ids"`
}

// CaseList is the GET /v1/cases response.
type CaseList struct {
	Items      []CaseSummary `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

// CaseFile is the v1 case document.
type CaseFile struct {
	SchemaVersion int              `json:"schema_version"`
	Case          CaseFileCase     `json:"case"`
	Breaks        []Break          `json:"breaks"`
	Proposals     []ProposalDetail `json:"proposals"`
	AgentRuns     []AgentRun       `json:"agent_runs"`
}

// CaseFileCase is the case section of the case file.
type CaseFileCase struct {
	ID               string     `json:"id"`
	Priority         int        `json:"priority"`
	ValueMinor       int64      `json:"value_minor"`
	Currency         string     `json:"currency"`
	Status           string     `json:"status"`
	Version          int        `json:"version"`
	Attempts         int        `json:"attempts"`
	EscalationReason *string    `json:"escalation_reason"`
	CreatedAt        time.Time  `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	ResolutionType   *string    `json:"resolution_type"`
	BreakIDs         []string   `json:"break_ids"`
}

// Break is one full break row.
type Break struct {
	ID           string            `json:"id"`
	Signature    string            `json:"signature"`
	ScopeID      string            `json:"scope_id"`
	MatchRunID   string            `json:"match_run_id"`
	Side         string            `json:"side"`
	LedgerTxnIDs []string          `json:"ledger_txn_ids"`
	ExternalRefs []match.RecordRef `json:"external_refs"`
	DeltaMinor   int64             `json:"delta_minor"`
	Currency     string            `json:"currency"`
	Status       string            `json:"status"`
	DetectedAt   time.Time         `json:"detected_at"`
}

// ProposalDetail is the case-file proposal section. It includes the sanitized
// rationale and evidence references but never raw evidence objects or payload.
type ProposalDetail struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	RiskTier        *string    `json:"risk_tier"`
	Confidence      float64    `json:"confidence"`
	PatternClaimed  *string    `json:"pattern_claimed"`
	PatternVerified *string    `json:"pattern_verified"`
	Rationale       string     `json:"rationale"`
	EvidenceRefs    []string   `json:"evidence_refs"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

// ProposalSummary is one row of GET /v1/proposals.
type ProposalSummary struct {
	ID              string     `json:"id"`
	CaseID          string     `json:"case_id"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	RiskTier        *string    `json:"risk_tier"`
	Confidence      float64    `json:"confidence"`
	PatternClaimed  *string    `json:"pattern_claimed"`
	PatternVerified *string    `json:"pattern_verified"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

// ProposalList is the GET /v1/proposals response.
type ProposalList struct {
	Items      []ProposalSummary `json:"items"`
	NextCursor string            `json:"next_cursor"`
}

// AgentRun is the bounded agent-run section of the case file.
type AgentRun struct {
	ID            string     `json:"id"`
	Model         string     `json:"model"`
	PromptVersion string     `json:"prompt_version"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
	Outcome       *string    `json:"outcome"`
	TokensIn      int64      `json:"tokens_in"`
	TokensOut     int64      `json:"tokens_out"`
	CostUSD       string     `json:"cost_usd"`
	TraceID       *string    `json:"trace_id"`
}

// IngestResult is the POST /v1/ingest/files response.
type IngestResult struct {
	BatchID          string `json:"batch_id"`
	Status           string `json:"status"`
	NoOp             bool   `json:"no_op"`
	Parsed           int    `json:"parsed"`
	Inserted         int    `json:"inserted"`
	Skipped          int    `json:"skipped"`
	QuarantineReason string `json:"quarantine_reason"`
}

// ReconcileResult is the POST /v1/reconcile/run response.
type ReconcileResult struct {
	RunID          string         `json:"run_id"`
	ScopeID        string         `json:"scope_id"`
	Stats          match.RunStats `json:"stats"`
	Breaks         int            `json:"breaks"`
	BreaksCreated  int            `json:"breaks_created"`
	BreaksExisting int            `json:"breaks_existing"`
	CasesCreated   int            `json:"cases_created"`
}

// SwitchRequest is the decoded POST /v1/admin/switches body.
type SwitchRequest struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

// SwitchResult is the POST /v1/admin/switches response.
type SwitchResult struct {
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	Changed   bool      `json:"changed"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CaseListQuery is the validated case-list filter.
type CaseListQuery struct {
	Status string
	Limit  int
	Cursor *listCursor
}

// CaseListPage is the store's case-list result.
type CaseListPage struct {
	Items      []CaseSummary
	NextCursor string
}

// ProposalListQuery is the validated proposal-list filter.
type ProposalListQuery struct {
	Status string
	CaseID string
	Limit  int
	Cursor *listCursor
}

// ProposalListPage is the store's proposal-list result.
type ProposalListPage struct {
	Items      []ProposalSummary
	NextCursor string
}

// SwitchSetRequest is the service-level switch mutation. ActorID is the API
// key ID that becomes human:<id>.
type SwitchSetRequest struct {
	Name    string
	Enabled bool
	Reason  string
	ActorID string
	TraceID string
}
