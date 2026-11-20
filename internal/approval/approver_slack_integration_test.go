//go:build integration

// covers: FIN-33
package approval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval"
)

func stringPtr(value string) *string { return &value }

// TestApproverBySlackUserID exercises the channel identity lookup against the
// real approvers table. The returned view carries no API key id or Slack id.
func TestApproverBySlackUserID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		spec        approverSpec
		slackUserID string
		wantRole    string
		wantErr     error
	}{
		{
			name:        "approver role",
			spec:        approverSpec{SlackUserID: stringPtr("U-APPROVER")},
			slackUserID: "U-APPROVER",
			wantRole:    "approver",
		},
		{
			name:        "admin role",
			spec:        approverSpec{Role: "admin", SlackUserID: stringPtr("U-ADMIN")},
			slackUserID: "U-ADMIN",
			wantRole:    "admin",
		},
		{
			name:        "unknown user",
			spec:        approverSpec{SlackUserID: stringPtr("U-OTHER")},
			slackUserID: "U-UNKNOWN",
			wantErr:     approval.ErrApproverNotFound,
		},
		{
			name:        "viewer role",
			spec:        approverSpec{Role: "viewer", SlackUserID: stringPtr("U-VIEWER")},
			slackUserID: "U-VIEWER",
			wantErr:     approval.ErrApproverRole,
		},
		{
			name:        "inactive approver",
			spec:        approverSpec{Active: boolPtr(false), SlackUserID: stringPtr("U-INACTIVE")},
			slackUserID: "U-INACTIVE",
			wantErr:     approval.ErrApproverInactive,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approverID, _ := f.seedApprover(test.spec)
			got, err := f.service.ApproverBySlackUserID(ctx, test.slackUserID)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				if got != (approval.Approver{}) {
					t.Fatalf("approver = %+v, want zero value", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApproverBySlackUserID: %v", err)
			}
			if got.ID != approverID || got.Role != test.wantRole || !got.Active {
				t.Fatalf("approver = %+v, want id=%s role=%s active", got, approverID, test.wantRole)
			}
		})
	}
}

func TestApproverBySlackUserIDRejectsMalformedInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, slackUserID := range []string{
		"",
		" U-PADDED ",
		strings.Repeat("U", 129),
	} {
		approver, err := f.service.ApproverBySlackUserID(ctx, slackUserID)
		if !errors.Is(err, approval.ErrInvalidRequest) {
			t.Fatalf("ApproverBySlackUserID(%q) error = %v, want ErrInvalidRequest", slackUserID, err)
		}
		if approver != (approval.Approver{}) {
			t.Fatalf("ApproverBySlackUserID(%q) = %+v, want zero value", slackUserID, approver)
		}
		if err != nil && slackUserID != "" && strings.Contains(err.Error(), slackUserID) {
			t.Fatalf("error echoes the rejected user id: %v", err)
		}
	}
}
