package traceability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/match"
)

// Reader errors. ErrNotFound is returned (wrapped) only when the root ledger
// transaction or case row is absent; every other absent link is reported in a
// chain's Missing slice instead.
var (
	// ErrNotFound reports an unknown root ledger transaction or case.
	ErrNotFound = errors.New("traceability: not found")
	// ErrInvalidID reports a root id that is not a canonical lowercase UUID.
	ErrInvalidID = errors.New("traceability: invalid id")
	// ErrInvalidData reports a stored JSON column the reader cannot decode.
	ErrInvalidData = errors.New("traceability: invalid stored data")
)

// Missing-link markers, in the spec 5.2 order. Missing arrays are sorted by
// this order, never lexicographically.
const (
	MissingAppliedProposal = "applied_proposal"
	MissingProposal        = "proposal"
	MissingCase            = "case"
	MissingBreaks          = "breaks"
	MissingAgentRun        = "agent_run"
	MissingAgentRunTraceID = "agent_run.trace_id"
	MissingVerification    = "verification"
	MissingPolicy          = "policy"
	MissingPolicyConfig    = "policy_config"
	MissingApprovals       = "approvals"
	MissingExecutorAudit   = "executor.audit"
	MissingLedgerTxn       = "ledger_txn"
	MissingLedgerEntries   = "ledger_entries"
)

var markerOrder = []string{
	MissingAppliedProposal,
	MissingProposal,
	MissingCase,
	MissingBreaks,
	MissingAgentRun,
	MissingAgentRunTraceID,
	MissingVerification,
	MissingPolicy,
	MissingPolicyConfig,
	MissingApprovals,
	MissingExecutorAudit,
	MissingLedgerTxn,
	MissingLedgerEntries,
}

// canonicalUUIDPattern is the lowercase 8-4-4-4-12 form used at every other
// ingress in this repository. A malformed id is a caller error, never an
// unknown root.
var canonicalUUIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Queryer is the read surface ByLedgerTxn and ByCase need. Both *pgxpool.Pool
// and pgx.Tx satisfy it. The interface deliberately has no Exec: the reader
// cannot write, even by mistake.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Trace is one resolved snapshot. LedgerTxn is set by ByLedgerTxn; Case is set
// by ByCase. Chains is always non-nil, as are each chain's arrays and Missing.
type Trace struct {
	LedgerTxn *LedgerTxn `json:"ledger_txn"`
	Case      *Case      `json:"case,omitempty"`
	Chains    []Chain    `json:"chains"`
	Missing   []string   `json:"missing"`
}

// Chain is one applied proposal and every link reachable from it. Nullable
// links stay in the chain with their missing marker instead of disappearing.
type Chain struct {
	// LedgerTxnID is applied_proposals.ledger_txn_id, which is null for an
	// application that did not post to the ledger.
	LedgerTxnID     *string          `json:"ledger_txn_id"`
	AppliedProposal *AppliedProposal `json:"applied_proposal"`
	Proposal        *Proposal        `json:"proposal"`
	Case            *Case            `json:"case"`
	Breaks          []Break          `json:"breaks"`
	AgentRun        *AgentRun        `json:"agent_run"`
	Verification    json.RawMessage  `json:"verification"`
	Policy          *Policy          `json:"policy"`
	Approvals       []Approval       `json:"approvals"`
	Executor        Executor         `json:"executor"`
	LedgerTxn       *LedgerTxn       `json:"ledger_txn"`
	LedgerEntries   []LedgerEntry    `json:"ledger_entries"`
	Missing         []string         `json:"missing"`
}

// AppliedProposal is the applied_proposals row that roots a chain.
type AppliedProposal struct {
	ProposalID    string    `json:"proposal_id"`
	LedgerTxnID   *string   `json:"ledger_txn_id"`
	AppliedAt     time.Time `json:"applied_at"`
	ExecutorRunID string    `json:"executor_run_id"`
}

// Proposal is the proposal link of the chain. Payload and rationale are not
// part of the trace; the case file carries those.
type Proposal struct {
	ID         string    `json:"id"`
	CaseID     string    `json:"case_id"`
	AgentRunID string    `json:"agent_run_id"`
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	RiskTier   *string   `json:"risk_tier"`
	CreatedAt  time.Time `json:"created_at"`
}

// Case is the case link of the chain.
type Case struct {
	ID             string     `json:"id"`
	Priority       int        `json:"priority"`
	ValueMinor     int64      `json:"value_minor"`
	Currency       string     `json:"currency"`
	Status         string     `json:"status"`
	Version        int        `json:"version"`
	Attempts       int        `json:"attempts"`
	Escalation     *string    `json:"escalation_reason"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	ResolutionType *string    `json:"resolution_type"`
}

// Break is one break linked to the chain's case through case_breaks.
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

// AgentRun is the proposal's agent run and its OTel trace id.
type AgentRun struct {
	ID            string     `json:"id"`
	Model         string     `json:"model"`
	PromptVersion string     `json:"prompt_version"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
	Outcome       *string    `json:"outcome"`
	TraceID       *string    `json:"trace_id"`
}

// Policy flattens the proposal's stored decision and the referenced
// policy_config_versions row. Config fields stay nil when the config row is
// absent while the decision fields remain present.
type Policy struct {
	RiskTier       *string    `json:"risk_tier"`
	Rule           string     `json:"rule"`
	Reasons        []string   `json:"reasons"`
	ConfigHash     string     `json:"config_hash"`
	ConfigYAML     *string    `json:"config_yaml"`
	ConfigLoadedAt *time.Time `json:"config_loaded_at"`

	// decisionTier drives the approvals marker. The policy object exposes
	// risk_tier, not the decision's tier, so the tier is kept internal.
	decisionTier string
}

// Approval is one approvals row plus its approver identity.
type Approval struct {
	ID             string    `json:"id"`
	Channel        string    `json:"channel"`
	ExternalRef    *string   `json:"external_ref"`
	Decision       string    `json:"decision"`
	DecidedAt      time.Time `json:"decided_at"`
	SignatureValid bool      `json:"signature_valid"`
	Approver       Approver  `json:"approver"`
}

// Approver is the approvals display join (approvers.display_name and role).
type Approver struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// Executor wraps the executor.applied audit row attributable to the proposal.
type Executor struct {
	Audit *ExecutorAudit `json:"audit"`
}

// ExecutorAudit is the executor.applied audit row. After carries
// ledger_txn_id, applied_at, and executor_run_id written by the executor.
type ExecutorAudit struct {
	Seq       int64           `json:"seq"`
	Timestamp time.Time       `json:"ts"`
	Actor     string          `json:"actor"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
}

// LedgerTxn is the ledger transaction link (root or applied target).
type LedgerTxn struct {
	ID            string    `json:"id"`
	Seq           int64     `json:"seq"`
	Kind          string    `json:"kind"`
	ValueDate     string    `json:"value_date"`
	CreatedAt     time.Time `json:"created_at"`
	BookedAt      time.Time `json:"booked_at"`
	Description   string    `json:"description"`
	PeriodID      string    `json:"period_id"`
	AdjustsPeriod *string   `json:"adjusts_period_id"`
	ReversalOf    *string   `json:"reversal_of"`
}

// LedgerEntry is one ledger_entries row with its account code.
type LedgerEntry struct {
	ID          string    `json:"id"`
	TxnID       string    `json:"txn_id"`
	AccountID   string    `json:"account_id"`
	AccountCode string    `json:"account_code"`
	Direction   string    `json:"direction"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	PostedAt    time.Time `json:"posted_at"`
	ExternalRef *string   `json:"external_ref"`
}

// ByLedgerTxn resolves the chain rooted at one ledger transaction. It returns
// ErrNotFound only when the ledger transaction row is absent; a transaction
// with no applied proposal is a successful trace with no chains and the
// applied_proposal marker.
func ByLedgerTxn(ctx context.Context, q Queryer, ledgerTxnID string) (Trace, error) {
	if err := validateRootID("ledger txn", ledgerTxnID); err != nil {
		return Trace{}, err
	}
	root, err := loadLedgerTxn(ctx, q, ledgerTxnID)
	if err != nil {
		return Trace{}, err
	}
	chains, err := loadChains(ctx, q, chainByLedgerTxnWhere, ledgerTxnID)
	if err != nil {
		return Trace{}, err
	}
	return assembleTrace(Trace{LedgerTxn: root}, chains, len(chains) == 0), nil
}

// ByCase resolves one chain per applied_proposals row for the case, including
// applications whose ledger_txn_id is null. It returns ErrNotFound only when
// the case row is absent.
func ByCase(ctx context.Context, q Queryer, caseID string) (Trace, error) {
	if err := validateRootID("case", caseID); err != nil {
		return Trace{}, err
	}
	root, err := loadCase(ctx, q, caseID)
	if err != nil {
		return Trace{}, err
	}
	chains, err := loadChains(ctx, q, chainByCaseWhere, caseID)
	if err != nil {
		return Trace{}, err
	}
	return assembleTrace(Trace{Case: root}, chains, len(chains) == 0), nil
}

func validateRootID(kind, id string) error {
	if !canonicalUUIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %s %q is not a canonical lowercase UUID", ErrInvalidID, kind, id)
	}
	return nil
}

// assembleTrace loads the auxiliary arrays for every chain (breaks, approvals,
// executor audit, ledger entries), computes markers, and imposes deterministic
// ordering. It issues the same number of statements regardless of chain count.
func assembleTrace(trace Trace, chains []Chain, rootWithoutApplications bool) Trace {
	trace.Chains = chains
	trace.Missing = unionMissing(chains)
	if rootWithoutApplications {
		trace.Missing = sortMarkers(append(trace.Missing, MissingAppliedProposal))
	}
	return trace
}

// ---------------------------------------------------------------- SQL loading

const chainSelect = `SELECT
        ap.proposal_id::text,
        ap.ledger_txn_id::text,
        ap.applied_at,
        ap.executor_run_id,
        p.id::text,
        p.case_id::text,
        p.agent_run_id::text,
        p.type,
        p.status,
        p.risk_tier,
        p.verification,
        p.policy_decision,
        p.created_at,
        c.id::text,
        c.priority,
        c.value_minor,
        c.currency,
        c.status,
        c.version,
        c.attempts,
        c.escalation_reason,
        c.created_at,
        c.resolved_at,
        c.resolution_type,
        ar.id::text,
        ar.model,
        ar.prompt_version,
        ar.started_at,
        ar.ended_at,
        ar.outcome,
        ar.trace_id,
        lt.id::text,
        lt.seq,
        lt.kind,
        lt.value_date::text,
        lt.created_at,
        lt.booked_at,
        lt.description,
        lt.period_id::text,
        lt.adjusts_period_id::text,
        lt.reversal_of::text,
        pcv.hash,
        pcv.yaml,
        pcv.loaded_at
    FROM applied_proposals ap
    LEFT JOIN proposals p ON p.id = ap.proposal_id
    LEFT JOIN cases c ON c.id = p.case_id
    LEFT JOIN agent_runs ar ON ar.id = p.agent_run_id
    LEFT JOIN ledger_txns lt ON lt.id = ap.ledger_txn_id
    LEFT JOIN policy_config_versions pcv
           ON pcv.hash = p.policy_decision->>'config_hash'`

const chainByLedgerTxnWhere = `
    WHERE ap.ledger_txn_id = $1::uuid
    ORDER BY ap.applied_at, ap.proposal_id`

const chainByCaseWhere = `
    WHERE p.case_id = $1::uuid
    ORDER BY ap.applied_at, ap.proposal_id`

type chainRow struct {
	appliedProposalID    string
	appliedLedgerTxnID   *string
	appliedAt            time.Time
	executorRunID        string
	proposalID           *string
	proposalCaseID       *string
	proposalAgentRunID   *string
	proposalType         *string
	proposalStatus       *string
	proposalRiskTier     *string
	proposalVerification []byte
	proposalDecision     []byte
	proposalCreatedAt    *time.Time
	caseID               *string
	casePriority         *int
	caseValueMinor       *int64
	caseCurrency         *string
	caseStatus           *string
	caseVersion          *int
	caseAttempts         *int
	caseEscalation       *string
	caseCreatedAt        *time.Time
	caseResolvedAt       *time.Time
	caseResolutionType   *string
	agentRunID           *string
	agentRunModel        *string
	agentRunPrompt       *string
	agentRunStartedAt    *time.Time
	agentRunEndedAt      *time.Time
	agentRunOutcome      *string
	agentRunTraceID      *string
	ledgerTxnID          *string
	ledgerTxnSeq         *int64
	ledgerTxnKind        *string
	ledgerTxnValueDate   *string
	ledgerTxnCreatedAt   *time.Time
	ledgerTxnBookedAt    *time.Time
	ledgerTxnDescription *string
	ledgerTxnPeriodID    *string
	ledgerTxnAdjusts     *string
	ledgerTxnReversalOf  *string
	configHash           *string
	configYAML           *string
	configLoadedAt       *time.Time
}

func (r *chainRow) scan(row pgx.Row) error {
	return row.Scan(
		&r.appliedProposalID,
		&r.appliedLedgerTxnID,
		&r.appliedAt,
		&r.executorRunID,
		&r.proposalID,
		&r.proposalCaseID,
		&r.proposalAgentRunID,
		&r.proposalType,
		&r.proposalStatus,
		&r.proposalRiskTier,
		&r.proposalVerification,
		&r.proposalDecision,
		&r.proposalCreatedAt,
		&r.caseID,
		&r.casePriority,
		&r.caseValueMinor,
		&r.caseCurrency,
		&r.caseStatus,
		&r.caseVersion,
		&r.caseAttempts,
		&r.caseEscalation,
		&r.caseCreatedAt,
		&r.caseResolvedAt,
		&r.caseResolutionType,
		&r.agentRunID,
		&r.agentRunModel,
		&r.agentRunPrompt,
		&r.agentRunStartedAt,
		&r.agentRunEndedAt,
		&r.agentRunOutcome,
		&r.agentRunTraceID,
		&r.ledgerTxnID,
		&r.ledgerTxnSeq,
		&r.ledgerTxnKind,
		&r.ledgerTxnValueDate,
		&r.ledgerTxnCreatedAt,
		&r.ledgerTxnBookedAt,
		&r.ledgerTxnDescription,
		&r.ledgerTxnPeriodID,
		&r.ledgerTxnAdjusts,
		&r.ledgerTxnReversalOf,
		&r.configHash,
		&r.configYAML,
		&r.configLoadedAt,
	)
}

// loadChains reads the applied-proposal spine and every directly joined link in
// one statement, then fills the one-to-many arrays in constant-count queries.
func loadChains(ctx context.Context, q Queryer, where, arg string) ([]Chain, error) {
	rows, err := q.Query(ctx, chainSelect+where, arg)
	if err != nil {
		return nil, fmt.Errorf("traceability: query chains: %w", err)
	}
	defer rows.Close()

	chains := make([]Chain, 0)
	for rows.Next() {
		var row chainRow
		if err := row.scan(rows); err != nil {
			return nil, fmt.Errorf("traceability: scan chain: %w", err)
		}
		chain, err := row.chain()
		if err != nil {
			return nil, err
		}
		chains = append(chains, chain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("traceability: read chains: %w", err)
	}
	if len(chains) == 0 {
		return chains, nil
	}

	if err := loadBreaks(ctx, q, chains); err != nil {
		return nil, err
	}
	if err := loadApprovals(ctx, q, chains); err != nil {
		return nil, err
	}
	if err := loadExecutorAudits(ctx, q, chains); err != nil {
		return nil, err
	}
	if err := loadLedgerEntries(ctx, q, chains); err != nil {
		return nil, err
	}

	for i := range chains {
		chains[i].Missing = chainMissing(&chains[i])
	}
	sortChains(chains)
	return chains, nil
}

func (r *chainRow) chain() (Chain, error) {
	chain := Chain{
		LedgerTxnID: r.appliedLedgerTxnID,
		AppliedProposal: &AppliedProposal{
			ProposalID:    r.appliedProposalID,
			LedgerTxnID:   r.appliedLedgerTxnID,
			AppliedAt:     r.appliedAt.UTC(),
			ExecutorRunID: r.executorRunID,
		},
		Breaks:        make([]Break, 0),
		Approvals:     make([]Approval, 0),
		LedgerEntries: make([]LedgerEntry, 0),
	}
	if r.proposalID != nil {
		chain.Proposal = &Proposal{
			ID:         *r.proposalID,
			CaseID:     deref(r.proposalCaseID),
			AgentRunID: deref(r.proposalAgentRunID),
			Type:       deref(r.proposalType),
			Status:     deref(r.proposalStatus),
			RiskTier:   r.proposalRiskTier,
			CreatedAt:  derefTime(r.proposalCreatedAt),
		}
	}
	if r.caseID != nil {
		chain.Case = &Case{
			ID:             *r.caseID,
			Priority:       derefInt(r.casePriority),
			ValueMinor:     derefInt64(r.caseValueMinor),
			Currency:       deref(r.caseCurrency),
			Status:         deref(r.caseStatus),
			Version:        derefInt(r.caseVersion),
			Attempts:       derefInt(r.caseAttempts),
			Escalation:     r.caseEscalation,
			CreatedAt:      derefTime(r.caseCreatedAt),
			ResolvedAt:     utcTimePtr(r.caseResolvedAt),
			ResolutionType: r.caseResolutionType,
		}
	}
	if r.agentRunID != nil {
		chain.AgentRun = &AgentRun{
			ID:            *r.agentRunID,
			Model:         deref(r.agentRunModel),
			PromptVersion: deref(r.agentRunPrompt),
			StartedAt:     derefTime(r.agentRunStartedAt),
			EndedAt:       utcTimePtr(r.agentRunEndedAt),
			Outcome:       r.agentRunOutcome,
			TraceID:       r.agentRunTraceID,
		}
	}
	if r.ledgerTxnID != nil {
		chain.LedgerTxn = &LedgerTxn{
			ID:            *r.ledgerTxnID,
			Seq:           derefInt64(r.ledgerTxnSeq),
			Kind:          deref(r.ledgerTxnKind),
			ValueDate:     deref(r.ledgerTxnValueDate),
			CreatedAt:     derefTime(r.ledgerTxnCreatedAt),
			BookedAt:      derefTime(r.ledgerTxnBookedAt),
			Description:   deref(r.ledgerTxnDescription),
			PeriodID:      deref(r.ledgerTxnPeriodID),
			AdjustsPeriod: r.ledgerTxnAdjusts,
			ReversalOf:    r.ledgerTxnReversalOf,
		}
	}
	if r.proposalDecision != nil {
		policy, err := decodePolicy(r.proposalDecision, r.proposalRiskTier,
			r.configYAML, r.configLoadedAt)
		if err != nil {
			return Chain{}, err
		}
		chain.Policy = policy
	}
	if len(r.proposalVerification) > 0 && !isJSONNull(r.proposalVerification) {
		if !json.Valid(r.proposalVerification) {
			return Chain{}, fmt.Errorf("%w: proposal %s verification is not valid JSON",
				ErrInvalidData, deref(r.proposalID))
		}
		chain.Verification = append(json.RawMessage(nil), r.proposalVerification...)
	}
	return chain, nil
}

// decisionDocument is the subset of policy.Decision the trace exposes.
type decisionDocument struct {
	Tier       string   `json:"tier"`
	Rule       string   `json:"rule"`
	Reasons    []string `json:"reasons"`
	ConfigHash string   `json:"config_hash"`
}

func decodePolicy(raw []byte, riskTier *string, configYAML *string,
	configLoadedAt *time.Time) (*Policy, error) {
	var decision decisionDocument
	if err := json.Unmarshal(raw, &decision); err != nil {
		return nil, fmt.Errorf("%w: policy_decision is not valid JSON: %v",
			ErrInvalidData, err)
	}
	reasons := decision.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	policy := &Policy{
		RiskTier:       riskTier,
		Rule:           decision.Rule,
		Reasons:        reasons,
		ConfigHash:     decision.ConfigHash,
		ConfigYAML:     configYAML,
		ConfigLoadedAt: utcTimePtr(configLoadedAt),
		decisionTier:   decision.Tier,
	}
	return policy, nil
}

func loadLedgerTxn(ctx context.Context, q Queryer, id string) (*LedgerTxn, error) {
	var (
		txn           LedgerTxn
		adjustsPeriod *string
		reversalOf    *string
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, seq, kind, value_date::text, created_at, booked_at,
		        description, period_id::text, adjusts_period_id::text,
		        reversal_of::text
		   FROM ledger_txns
		  WHERE id = $1::uuid`, id).
		Scan(&txn.ID, &txn.Seq, &txn.Kind, &txn.ValueDate, &txn.CreatedAt,
			&txn.BookedAt, &txn.Description, &txn.PeriodID, &adjustsPeriod,
			&reversalOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: ledger txn %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("traceability: read ledger txn %s: %w", id, err)
	}
	txn.CreatedAt = txn.CreatedAt.UTC()
	txn.BookedAt = txn.BookedAt.UTC()
	txn.AdjustsPeriod = adjustsPeriod
	txn.ReversalOf = reversalOf
	return &txn, nil
}

func loadCase(ctx context.Context, q Queryer, id string) (*Case, error) {
	var (
		c              Case
		escalation     *string
		resolvedAt     *time.Time
		resolutionType *string
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, priority, value_minor, currency, status, version,
		        attempts, escalation_reason, created_at, resolved_at,
		        resolution_type
		   FROM cases
		  WHERE id = $1::uuid`, id).
		Scan(&c.ID, &c.Priority, &c.ValueMinor, &c.Currency, &c.Status,
			&c.Version, &c.Attempts, &escalation, &c.CreatedAt, &resolvedAt,
			&resolutionType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: case %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("traceability: read case %s: %w", id, err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	c.Escalation = escalation
	c.ResolvedAt = utcTimePtr(resolvedAt)
	c.ResolutionType = resolutionType
	return &c, nil
}

func loadBreaks(ctx context.Context, q Queryer, chains []Chain) error {
	caseIDs := make([]string, 0, len(chains))
	for i := range chains {
		if chains[i].Case != nil {
			caseIDs = append(caseIDs, chains[i].Case.ID)
		}
	}
	if len(caseIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx,
		`SELECT cb.case_id::text, b.id::text, b.signature, b.scope_id::text,
		        b.match_run_id::text, b.side, b.ledger_txn_ids::text[],
		        b.external_refs, b.delta_minor, b.currency, b.status,
		        b.detected_at
		   FROM case_breaks cb
		   JOIN breaks b ON b.id = cb.break_id
		  WHERE cb.case_id = ANY($1::uuid[])
		  ORDER BY cb.case_id, b.detected_at, b.id`, caseIDs)
	if err != nil {
		return fmt.Errorf("traceability: query breaks: %w", err)
	}
	defer rows.Close()

	byCase := make(map[string][]Break, len(caseIDs))
	for rows.Next() {
		var (
			caseID     string
			breakRow   Break
			externalJS []byte
		)
		if err := rows.Scan(&caseID, &breakRow.ID, &breakRow.Signature,
			&breakRow.ScopeID, &breakRow.MatchRunID, &breakRow.Side,
			&breakRow.LedgerTxnIDs, &externalJS, &breakRow.DeltaMinor,
			&breakRow.Currency, &breakRow.Status, &breakRow.DetectedAt); err != nil {
			return fmt.Errorf("traceability: scan break: %w", err)
		}
		if err := json.Unmarshal(externalJS, &breakRow.ExternalRefs); err != nil {
			return fmt.Errorf("%w: break %s external_refs: %v",
				ErrInvalidData, breakRow.ID, err)
		}
		if breakRow.ExternalRefs == nil {
			breakRow.ExternalRefs = []match.RecordRef{}
		}
		if breakRow.LedgerTxnIDs == nil {
			breakRow.LedgerTxnIDs = []string{}
		}
		breakRow.DetectedAt = breakRow.DetectedAt.UTC()
		byCase[caseID] = append(byCase[caseID], breakRow)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("traceability: read breaks: %w", err)
	}
	for i := range chains {
		if chains[i].Case == nil {
			continue
		}
		if breaks, ok := byCase[chains[i].Case.ID]; ok {
			chains[i].Breaks = breaks
		}
	}
	return nil
}

func loadApprovals(ctx context.Context, q Queryer, chains []Chain) error {
	proposalIDs := proposalIDsWithIndices(chains, func(i int) bool {
		return chains[i].Proposal != nil
	})
	if len(proposalIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx,
		`SELECT a.proposal_id::text, a.id::text, a.channel, a.external_ref,
		        a.decision, a.decided_at, a.signature_valid,
		        av.id::text, av.display_name, av.role
		   FROM approvals a
		   LEFT JOIN approvers av ON av.id = a.approver_id
		  WHERE a.proposal_id = ANY($1::uuid[])
		  ORDER BY a.decided_at, a.id`, proposalIDs)
	if err != nil {
		return fmt.Errorf("traceability: query approvals: %w", err)
	}
	defer rows.Close()

	byProposal := make(map[string][]Approval, len(proposalIDs))
	for rows.Next() {
		var (
			proposalID   string
			approval     Approval
			approverID   *string
			displayName  *string
			approverRole *string
		)
		if err := rows.Scan(&proposalID, &approval.ID, &approval.Channel,
			&approval.ExternalRef, &approval.Decision, &approval.DecidedAt,
			&approval.SignatureValid, &approverID, &displayName,
			&approverRole); err != nil {
			return fmt.Errorf("traceability: scan approval: %w", err)
		}
		approval.DecidedAt = approval.DecidedAt.UTC()
		approval.Approver = Approver{
			ID:          deref(approverID),
			DisplayName: deref(displayName),
			Role:        deref(approverRole),
		}
		byProposal[proposalID] = append(byProposal[proposalID], approval)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("traceability: read approvals: %w", err)
	}
	for i := range chains {
		if chains[i].Proposal == nil {
			continue
		}
		if approvals, ok := byProposal[chains[i].Proposal.ID]; ok {
			chains[i].Approvals = approvals
		}
	}
	return nil
}

func loadExecutorAudits(ctx context.Context, q Queryer, chains []Chain) error {
	proposalIDs := proposalIDsWithIndices(chains, func(i int) bool {
		return chains[i].Proposal != nil
	})
	if len(proposalIDs) == 0 {
		return nil
	}

	// One apply per proposal (applied_proposals is keyed by proposal_id), so
	// the lowest-seq matching audit row is the attributable summary entry.
	rows, err := q.Query(ctx,
		`SELECT DISTINCT ON (entity_id)
		        entity_id, seq, ts, actor_type || ':' || actor_id, before, after
		   FROM audit_log
		  WHERE action = 'executor.applied'
		    AND entity_type = 'proposal'
		    AND entity_id = ANY($1::text[])
		  ORDER BY entity_id, seq`, proposalIDs)
	if err != nil {
		return fmt.Errorf("traceability: query executor audit: %w", err)
	}
	defer rows.Close()

	byProposal := make(map[string]*ExecutorAudit, len(proposalIDs))
	for rows.Next() {
		var (
			proposalID string
			audit      ExecutorAudit
			before     []byte
			after      []byte
		)
		if err := rows.Scan(&proposalID, &audit.Seq, &audit.Timestamp,
			&audit.Actor, &before, &after); err != nil {
			return fmt.Errorf("traceability: scan executor audit: %w", err)
		}
		audit.Timestamp = audit.Timestamp.UTC()
		audit.Before = rawJSON(before)
		audit.After = rawJSON(after)
		byProposal[proposalID] = &audit
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("traceability: read executor audit: %w", err)
	}
	for i := range chains {
		if chains[i].Proposal == nil {
			continue
		}
		if audit, ok := byProposal[chains[i].Proposal.ID]; ok {
			chains[i].Executor.Audit = audit
		}
	}
	return nil
}

func loadLedgerEntries(ctx context.Context, q Queryer, chains []Chain) error {
	txnIDs := make([]string, 0, len(chains))
	for i := range chains {
		if chains[i].LedgerTxn != nil {
			txnIDs = append(txnIDs, chains[i].LedgerTxn.ID)
		}
	}
	if len(txnIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx,
		`SELECT e.txn_id::text, e.id::text, e.account_id::text, a.code,
		        e.direction, e.amount_minor, e.currency, e.posted_at,
		        e.external_ref
		   FROM ledger_entries e
		   JOIN accounts a ON a.id = e.account_id
		  WHERE e.txn_id = ANY($1::uuid[])
		  ORDER BY e.txn_id, e.posted_at, e.id`, txnIDs)
	if err != nil {
		return fmt.Errorf("traceability: query ledger entries: %w", err)
	}
	defer rows.Close()

	byTxn := make(map[string][]LedgerEntry, len(txnIDs))
	for rows.Next() {
		var (
			txnID string
			entry LedgerEntry
		)
		if err := rows.Scan(&txnID, &entry.ID, &entry.AccountID,
			&entry.AccountCode, &entry.Direction, &entry.AmountMinor,
			&entry.Currency, &entry.PostedAt, &entry.ExternalRef); err != nil {
			return fmt.Errorf("traceability: scan ledger entry: %w", err)
		}
		entry.TxnID = txnID
		entry.PostedAt = entry.PostedAt.UTC()
		byTxn[txnID] = append(byTxn[txnID], entry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("traceability: read ledger entries: %w", err)
	}
	for i := range chains {
		if chains[i].LedgerTxn == nil {
			continue
		}
		if entries, ok := byTxn[chains[i].LedgerTxn.ID]; ok {
			chains[i].LedgerEntries = entries
		}
	}
	return nil
}

// proposalIDsWithIndices returns the distinct proposal ids selected by keep.
func proposalIDsWithIndices(chains []Chain, keep func(int) bool) []string {
	seen := make(map[string]struct{}, len(chains))
	ids := make([]string, 0, len(chains))
	for i := range chains {
		if !keep(i) {
			continue
		}
		id := chains[i].Proposal.ID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// ------------------------------------------------------- pure builders/order

// chainMissing computes one chain's markers in spec 5.2 order.
func chainMissing(chain *Chain) []string {
	missing := make([]string, 0, len(markerOrder))
	if chain.AppliedProposal == nil {
		missing = append(missing, MissingAppliedProposal)
	}
	if chain.Proposal == nil {
		missing = append(missing, MissingProposal)
	}
	if chain.Case == nil {
		missing = append(missing, MissingCase)
	}
	if len(chain.Breaks) == 0 {
		missing = append(missing, MissingBreaks)
	}
	switch {
	case chain.AgentRun == nil:
		missing = append(missing, MissingAgentRun)
	case chain.AgentRun.TraceID == nil || strings.TrimSpace(*chain.AgentRun.TraceID) == "":
		missing = append(missing, MissingAgentRunTraceID)
	}
	if len(chain.Verification) == 0 || isJSONNull(chain.Verification) {
		missing = append(missing, MissingVerification)
	}
	if chain.Policy == nil {
		missing = append(missing, MissingPolicy)
	} else {
		if chain.Policy.ConfigYAML == nil {
			missing = append(missing, MissingPolicyConfig)
		}
		if policyRequiresApproval(chain.Policy) && len(chain.Approvals) == 0 {
			missing = append(missing, MissingApprovals)
		}
	}
	if chain.Executor.Audit == nil {
		missing = append(missing, MissingExecutorAudit)
	}
	if chain.LedgerTxn == nil {
		missing = append(missing, MissingLedgerTxn)
	} else if len(chain.LedgerEntries) == 0 {
		missing = append(missing, MissingLedgerEntries)
	}
	return sortMarkers(missing)
}

// policyRequiresApproval reports whether the stored decision routed through
// human approval. T0 is auto-apply, so an absent approvals array is complete.
func policyRequiresApproval(policy *Policy) bool {
	switch policy.decisionTier {
	case "T1", "T2":
		return true
	default:
		return false
	}
}

// unionMissing is the sorted, deduplicated union of every chain's markers. A
// root without applications adds applied_proposal via assembleTrace.
func unionMissing(chains []Chain) []string {
	missing := make([]string, 0, len(markerOrder))
	for i := range chains {
		missing = append(missing, chains[i].Missing...)
	}
	return sortMarkers(missing)
}

// sortMarkers deduplicates and sorts markers by spec 5.2 order. The input is
// never mutated.
func sortMarkers(markers []string) []string {
	seen := make(map[string]struct{}, len(markers))
	out := make([]string, 0, len(markers))
	for _, marker := range markers {
		if _, ok := seen[marker]; ok {
			continue
		}
		seen[marker] = struct{}{}
		out = append(out, marker)
	}
	slices.SortStableFunc(out, func(a, b string) int {
		return markerRank(a) - markerRank(b)
	})
	return out
}

func markerRank(marker string) int {
	for i, known := range markerOrder {
		if marker == known {
			return i
		}
	}
	return len(markerOrder)
}

// sortChains orders by applied-proposal creation time with proposal id as the
// tie-break, so two applications with the same timestamp are stable.
func sortChains(chains []Chain) {
	slices.SortStableFunc(chains, func(a, b Chain) int {
		at, bt := time.Time{}, time.Time{}
		aid, bid := "", ""
		if a.AppliedProposal != nil {
			at = a.AppliedProposal.AppliedAt
			aid = a.AppliedProposal.ProposalID
		}
		if b.AppliedProposal != nil {
			bt = b.AppliedProposal.AppliedAt
			bid = b.AppliedProposal.ProposalID
		}
		if !at.Equal(bt) {
			if at.Before(bt) {
				return -1
			}
			return 1
		}
		switch {
		case aid < bid:
			return -1
		case aid > bid:
			return 1
		default:
			return 0
		}
	})
}

// ---------------------------------------------------------------- helpers

func isJSONNull(raw []byte) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func rawJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
