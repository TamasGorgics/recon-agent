package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// The persisted e2e result schema and its location in eval_case_results. The
// detail is owned by this package: the runner merges Detail() into the case
// detail, or calls WriteCaseDetail after the row exists.
const (
	// SchemaVersion is the only result schema this package emits.
	SchemaVersion = "eval-e2e/v1"
	// DetailKey is the eval_case_results.detail object key.
	DetailKey = "e2e"
	// Suite is the canonical eval_runs.suite value for e2e runs.
	Suite = "e2e"
	// ConfigKey is the eval_runs.config key that marks an e2e run.
	ConfigKey = "e2e"
)

// Proposal markers.
const (
	ProposalPresent = "present"
	ProposalNone    = "none"
)

// RequestStatus values. apply_failed is deliberately not a result status: the
// schema names the state the request reached (approved or auto_approved) and
// Failed/Error carry the apply failure. This keeps the result enum stable for
// reports.
const (
	RequestStatusNone            = "none"
	RequestStatusAutoApproved    = "auto_approved"
	RequestStatusPendingApproval = "pending_approval"
	RequestStatusApproved        = "approved"
	RequestStatusApplied         = "applied"
	RequestStatusRejected        = "rejected"
	RequestStatusExpired         = "expired"
	RequestStatusSuperseded      = "superseded"
)

// Approver action values. They mirror eval/approver.Action plus "none" for a
// result that did not need an approver step.
const (
	ActionNone    = "none"
	ActionSkipT0  = "skip_t0"
	ActionApprove = "approve"
	ActionReject  = "reject"
	ActionAbstain = "abstain"
)

const dateLayout = "2006-01-02"

var (
	canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// ErrNoCaseResult means WriteCaseDetail found no eval_case_results row.
	ErrNoCaseResult = errors.New("eval/e2e: eval_case_results row not found")
)

// Result is the eval-e2e/v1 document persisted at detail["e2e"]. All money is
// integer minor units; no floating-point money is emitted.
type Result struct {
	Schema           string    `json:"schema"`
	Proposal         string    `json:"proposal"`
	ProposalID       string    `json:"proposal_id,omitempty"`
	RequestStatus    string    `json:"request_status"`
	PolicyTier       *string   `json:"policy_tier"`
	AgentClaimedTier *string   `json:"agent_claimed_tier"`
	ApproverAction   string    `json:"approver_action"`
	ApproverRule     string    `json:"approver_rule"`
	Applied          bool      `json:"applied"`
	LedgerTxnID      string    `json:"ledger_txn_id,omitempty"`
	Tieout           *TieOut   `json:"tieout,omitempty"`
	Verifier         *Verifier `json:"verifier,omitempty"`
	Failed           bool      `json:"failed"`
	Error            string    `json:"error"`
}

// TieOut is the persisted tie-out summary. Currency is the scope currency and
// every amount is in integer minor units.
type TieOut struct {
	AsOf                  string `json:"as_of"`
	Currency              string `json:"currency"`
	UnexplainedMinor      int64  `json:"unexplained_minor"`
	LedgerOnlyMinor       int64  `json:"ledger_only_minor"`
	ExternalOnlyMinor     int64  `json:"external_only_minor"`
	LedgerBalanceMinor    int64  `json:"ledger_balance_minor"`
	StatementClosingMinor int64  `json:"statement_closing_minor"`
}

// Verifier is the observable verifier projection. Passed is true only when
// proposals.pattern_verified is non-null; Evaluated is true when either
// verification or pattern_verified is non-null. FalsePass is filled by the
// caller that owns equivalence scoring (W2b/score); this package never reads
// truth and therefore cannot derive it.
type Verifier struct {
	Evaluated bool   `json:"evaluated"`
	Passed    bool   `json:"passed"`
	Pattern   string `json:"pattern"`
	FalsePass bool   `json:"false_pass"`
}

// Detail returns the exact eval_case_results.detail fragment. The runner merges
// it like:
//
//	for key, value := range result.Detail() { detail[key] = value }
func (r Result) Detail() map[string]any {
	return map[string]any{DetailKey: r}
}

// DetailJSON returns the marshalled result object for callers that write JSONB
// directly. The bytes are stable for unchanged results (struct field order).
func (r Result) DetailJSON() ([]byte, error) {
	return json.Marshal(r)
}

// Validate enforces the closed schema vocabulary before the result is
// persisted or returned. It never inspects payloads or truth.
func (r Result) Validate() error {
	if r.Schema != SchemaVersion {
		return fmt.Errorf("eval/e2e: result schema %q is not %q", r.Schema, SchemaVersion)
	}
	switch r.Proposal {
	case ProposalPresent:
		if !canonicalUUIDPattern.MatchString(r.ProposalID) {
			return errors.New("eval/e2e: present result has no canonical proposal id")
		}
	case ProposalNone:
		if r.ProposalID != "" {
			return errors.New("eval/e2e: no-proposal result carries a proposal id")
		}
	default:
		return fmt.Errorf("eval/e2e: unknown proposal marker %q", r.Proposal)
	}
	if !validRequestStatus(r.RequestStatus) {
		return fmt.Errorf("eval/e2e: unknown request status %q", r.RequestStatus)
	}
	if r.Proposal == ProposalNone && r.RequestStatus != RequestStatusNone {
		return errors.New("eval/e2e: no-proposal result must have request_status none")
	}
	if r.PolicyTier != nil && !validTier(*r.PolicyTier) {
		return fmt.Errorf("eval/e2e: unknown policy tier %q", *r.PolicyTier)
	}
	if r.AgentClaimedTier != nil && !validTier(*r.AgentClaimedTier) {
		return fmt.Errorf("eval/e2e: unknown agent-claimed tier %q", *r.AgentClaimedTier)
	}
	if !validAction(r.ApproverAction) {
		return fmt.Errorf("eval/e2e: unknown approver action %q", r.ApproverAction)
	}
	if r.Applied && r.RequestStatus != RequestStatusApplied {
		return errors.New("eval/e2e: applied result must have request_status applied")
	}
	if r.LedgerTxnID != "" && !canonicalUUIDPattern.MatchString(r.LedgerTxnID) {
		return errors.New("eval/e2e: ledger txn id is not a canonical UUID")
	}
	if r.Failed && r.Error == "" {
		return errors.New("eval/e2e: failed result has an empty error")
	}
	if !r.Failed && r.Error != "" {
		return errors.New("eval/e2e: successful result carries an error")
	}
	if r.Tieout != nil {
		if _, err := time.Parse(dateLayout, r.Tieout.AsOf); err != nil {
			return fmt.Errorf("eval/e2e: tie-out as_of %q: %w", r.Tieout.AsOf, err)
		}
		if r.Tieout.Currency == "" {
			return errors.New("eval/e2e: tie-out has no currency")
		}
	}
	return nil
}

func validRequestStatus(status string) bool {
	switch status {
	case RequestStatusNone, RequestStatusAutoApproved, RequestStatusPendingApproval,
		RequestStatusApproved, RequestStatusApplied, RequestStatusRejected,
		RequestStatusExpired, RequestStatusSuperseded:
		return true
	default:
		return false
	}
}

func validTier(tier string) bool {
	switch tier {
	case "T0", "T1", "T2":
		return true
	default:
		return false
	}
}

func validAction(action string) bool {
	switch action {
	case ActionNone, ActionSkipT0, ActionApprove, ActionReject, ActionAbstain:
		return true
	default:
		return false
	}
}

// tieOutFrom converts the internal tie-out result into the persisted shape.
func tieOutFrom(result tieout.Result) *TieOut {
	return &TieOut{
		AsOf:                  result.AsOf.UTC().Format(dateLayout),
		Currency:              result.Currency,
		UnexplainedMinor:      result.Unexplained.AmountMinor(),
		LedgerOnlyMinor:       result.LedgerOnly.AmountMinor(),
		ExternalOnlyMinor:     result.ExternalOnly.AmountMinor(),
		LedgerBalanceMinor:    result.LedgerBalance.AmountMinor(),
		StatementClosingMinor: result.StatementClosing.AmountMinor(),
	}
}

// DetailExecer is the narrow write surface WriteCaseDetail needs.
// *pgxpool.Pool and pgx.Tx satisfy it.
type DetailExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// WriteCaseDetail persists the e2e result into an existing
// eval_case_results row. It is idempotent: the key is overwritten with the
// same or a resumed result. A missing row is an error, never a silent skip.
func WriteCaseDetail(ctx context.Context, execer DetailExecer, runID, scenarioID string, result Result) error {
	if execer == nil {
		return errors.New("eval/e2e: detail execer is required")
	}
	if !canonicalUUIDPattern.MatchString(runID) {
		return fmt.Errorf("eval/e2e: eval run id %q is not a canonical lowercase UUID", runID)
	}
	if scenarioID == "" {
		return errors.New("eval/e2e: scenario id is required")
	}
	if err := result.Validate(); err != nil {
		return err
	}
	document, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("eval/e2e: marshal result: %w", err)
	}
	tag, err := execer.Exec(ctx,
		`UPDATE eval_case_results
		    SET detail = jsonb_set(detail, '{e2e}', $3::jsonb, true)
		  WHERE eval_run_id = $1::uuid AND scenario_id = $2`,
		runID, scenarioID, document)
	if err != nil {
		return fmt.Errorf("eval/e2e: write case detail: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %s scenario %s", ErrNoCaseResult, runID, scenarioID)
	}
	return nil
}
