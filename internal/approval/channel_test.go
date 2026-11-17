package approval_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// covers: FIN-33
func TestFakeChannelRecordsAndIsDeterministic(t *testing.T) {
	channel := approval.NewFakeChannel()
	ctx := context.Background()
	req := approval.ApprovalRequest{
		ProposalID:     "00000000-0000-4000-8000-000000000001",
		CaseID:         "00000000-0000-4000-8000-000000000002",
		Channel:        approval.ChannelSlack,
		Tier:           policy.TierT1,
		Payload:        []byte(`{"proposal_id":"00000000-0000-4000-8000-000000000001"}`),
		IdempotencyKey: "approval.request:00000000-0000-4000-8000-000000000001",
	}

	first, err := channel.RequestApproval(ctx, req)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	second, err := channel.RequestApproval(ctx, req)
	if err != nil {
		t.Fatalf("second RequestApproval: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("external refs = %q/%q, want a deterministic non-empty value", first, second)
	}
	if got := channel.Requests(); len(got) != 2 || !reflect.DeepEqual(got[0], req) {
		t.Fatalf("recorded requests = %+v, want two copies of the request", got)
	}

	// The recorded payload is a copy; mutating the caller's slice cannot change
	// the recorded request.
	req.Payload[0] = 'X'
	if got := channel.Requests()[0]; string(got.Payload) == string(req.Payload) {
		t.Fatal("recorded request payload aliases the caller's slice")
	}

	if err := channel.Cancel(ctx, first); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := channel.Cancels(); len(got) != 1 || got[0] != first {
		t.Fatalf("recorded cancels = %v, want [%s]", got, first)
	}

	// Callback is fail-closed until configured.
	if _, err := channel.HandleCallback(ctx, &http.Request{}); !errors.Is(err, approval.ErrInvalidRequest) {
		t.Fatalf("unconfigured callback error = %v, want ErrInvalidRequest", err)
	}
	wantCallback := approval.CallbackDecision{
		ProposalID: req.ProposalID,
		Decision:   approval.DecisionApproved,
		ApproverID: "00000000-0000-4000-8000-000000000003",
		Source:     approval.ChannelSlack,
	}
	channel.SetCallback(wantCallback, nil)
	gotCallback, err := channel.HandleCallback(ctx, &http.Request{})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if gotCallback != wantCallback {
		t.Fatalf("callback = %+v, want %+v", gotCallback, wantCallback)
	}
	if channel.CallbackCalls() != 2 {
		t.Fatalf("callback calls = %d, want 2", channel.CallbackCalls())
	}
}

// covers: FIN-33
func TestDecisionValuesAreClosed(t *testing.T) {
	for _, value := range []approval.Decision{approval.DecisionApproved, approval.DecisionRejected} {
		if !value.Valid() {
			t.Errorf("%q is not valid", value)
		}
	}
	for _, value := range []approval.Decision{"", "approve", "APPROVED", "maybe"} {
		if value.Valid() {
			t.Errorf("%q is valid, want rejected", value)
		}
	}
}
