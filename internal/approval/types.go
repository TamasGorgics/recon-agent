package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// Approval channels. Request routes T1 to slack and T2 to ticket; M4-12 owns
// the adapters that actually send those messages.
const (
	ChannelSlack  = "slack"
	ChannelTicket = "ticket"
)

// RequestResult is the outcome of routing one proposed proposal. Status is the
// stored status after the transaction (auto_approved or pending_approval).
// Channel is empty for a T0 auto-approval. LimitsReason is the exact FIN-53
// limits reason for a T0 candidate (within_limits, daily_cap_exceeded, ...) and
// empty when limits were not consulted.
type RequestResult struct {
	ProposalID   string
	Tier         policy.Tier
	Status       string
	Channel      string
	ExpiresAt    time.Time
	AutoApproved bool
	LimitsReason string
}

// DecisionOptions carries the additive inputs of a decision. The zero value is
// the frozen M4-11 behavior. Reason is optional context; InfoRequested turns a
// rejection into a request for more information, which must not consume a case
// attempt or escalate.
type DecisionOptions struct {
	// Reason is optional human context. It is trimmed, capped at 2000 runes,
	// and rejected if it contains control or format characters. For a normal
	// rejection it becomes the proposal transition reason; for an approval it
	// is audit-only; for InfoRequested it is audit-only and the transition
	// reason is the constant "info_requested".
	Reason string
	// InfoRequested requires DecisionRejected. It selects the case lifecycle's
	// RequestInfo hook instead of OnProposalClosed.
	InfoRequested bool
}

// DecisionResult is the outcome of a human decision. Replayed is true when the
// same approver repeated the same decision and the service wrote nothing;
// DecidedAt is then the stored decision time.
type DecisionResult struct {
	ProposalID string
	Status     string
	Decision   Decision
	ApproverID string
	DecidedAt  time.Time
	Replayed   bool
}

// ApprovalRequest is one routed approval request handed to a channel adapter.
// Payload is the same JSON document stored on the approval.request outbox row
// (proposal_id, case_id, tier, channel, requested_at).
type ApprovalRequest struct {
	ProposalID     string
	CaseID         string
	Channel        string
	Tier           policy.Tier
	Payload        json.RawMessage
	IdempotencyKey string
}

// CallbackDecision is a decision delivered by a channel callback. The callback
// is untrusted input: M4-12 verifies the signature and identity before
// constructing one. The type is named CallbackDecision because the package's
// Decision is the closed approved/rejected enum.
type CallbackDecision struct {
	ProposalID string
	Decision   Decision
	ApproverID string
	Source     string
	Reason     string
}

// Approver is the public identity view returned by ApproverBySlackUserID and
// other channel identity lookups. It deliberately omits api_key_id and the
// channel-specific external identifiers: callers may act as this approver but
// never receive credential material.
type Approver struct {
	ID     string
	Role   string
	Active bool
}

// Channel delivers approval requests, decodes callbacks, and withdraws a
// request that became moot. M4-11 only defines the interface and FakeChannel;
// production adapters are M4-12. HandleCallback receives the original HTTP
// request so a signature-verifying adapter can authenticate the body.
type Channel interface {
	RequestApproval(ctx context.Context, req ApprovalRequest) (externalRef string, err error)
	HandleCallback(ctx context.Context, req *http.Request) (CallbackDecision, error)
	Cancel(ctx context.Context, externalRef string) error
}
