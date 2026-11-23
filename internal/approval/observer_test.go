package approval

import (
	"context"
	"fmt"
	"testing"
)

func TestDecisionErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ErrorCodeInternal},
		{"context canceled", context.Canceled, ErrorCodeTimeout},
		{"deadline", context.DeadlineExceeded, ErrorCodeTimeout},
		{"wrapped deadline", fmt.Errorf("decision: %w", context.DeadlineExceeded), ErrorCodeTimeout},
		{"invalid decision", ErrInvalidDecision, ErrorCodeInvalidRequest},
		{"invalid request", fmt.Errorf("%w: bad id", ErrInvalidRequest), ErrorCodeInvalidRequest},
		{"proposal not found", ErrProposalNotFound, ErrorCodeNotFound},
		{"approver not found", ErrApproverNotFound, ErrorCodeNotFound},
		{"approver role", ErrApproverRole, ErrorCodeForbidden},
		{"approver inactive", ErrApproverInactive, ErrorCodeForbidden},
		{"agent actor", ErrAgentActor, ErrorCodeForbidden},
		{"not pending", ErrNotPending, ErrorCodeConflict},
		{"expired", ErrExpired, ErrorCodeConflict},
		{"decision conflict", ErrDecisionConflict, ErrorCodeConflict},
		{"co-maker", ErrCoMaker, ErrorCodeConflict},
		{"info requested unsupported", ErrInfoRequestedUnsupported, ErrorCodeInternal},
		{"raw", fmt.Errorf("approval: database unavailable"), ErrorCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decisionErrorCode(test.err); got != test.want {
				t.Fatalf("decisionErrorCode(%v) = %q, want %q", test.err, got, test.want)
			}
		})
	}
}
