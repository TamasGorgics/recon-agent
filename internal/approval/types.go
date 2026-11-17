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

// Channel delivers approval requests, decodes callbacks, and withdraws a
// request that became moot. M4-11 only defines the interface and FakeChannel;
// production adapters are M4-12. HandleCallback receives the original HTTP
// request so a signature-verifying adapter can authenticate the body.
type Channel interface {
	RequestApproval(ctx context.Context, req ApprovalRequest) (externalRef string, err error)
	HandleCallback(ctx context.Context, req *http.Request) (CallbackDecision, error)
	Cancel(ctx context.Context, externalRef string) error
}
