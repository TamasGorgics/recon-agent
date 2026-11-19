package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const testMatchID = "44444444-4444-4444-8444-444444444444"

func TestParseUnmatchInputValidation(t *testing.T) {
	tests := []struct {
		name    string
		matchID string
		reason  string
		actor   string
		wantErr error
	}{
		{name: "empty match id", reason: "operator requested",
			actor: testHumanID, wantErr: ErrInvalidRequest},
		{name: "malformed match id", matchID: "not-a-uuid",
			reason: "operator requested", actor: testHumanID, wantErr: ErrInvalidRequest},
		{name: "empty actor", matchID: testMatchID,
			reason: "operator requested", wantErr: ErrInvalidActor},
		{name: "unknown actor prefix", matchID: testMatchID,
			reason: "operator requested", actor: "operator:alice", wantErr: ErrInvalidActor},
		{name: "actor with internal whitespace", matchID: testMatchID,
			reason: "operator requested", actor: "human:al ice", wantErr: ErrInvalidActor},
		{name: "system actor", matchID: testMatchID,
			reason: "operator requested", actor: "system:reconciler", wantErr: ErrActorNotAllowed},
		{name: "agent actor", matchID: testMatchID,
			reason: "operator requested", actor: "agent:model-v1", wantErr: ErrActorNotAllowed},
		{name: "empty reason", matchID: testMatchID, actor: testHumanID,
			wantErr: ErrInvalidReason},
		{name: "blank reason", matchID: testMatchID, reason: " \t ",
			actor: testHumanID, wantErr: ErrInvalidReason},
		{name: "overlong reason", matchID: testMatchID,
			reason: strings.Repeat("a", maxUnmatchReasonBytes+1),
			actor:  testHumanID, wantErr: ErrInvalidReason},
		{name: "control character reason", matchID: testMatchID,
			reason: "operator\x00requested", actor: testHumanID, wantErr: ErrInvalidReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseUnmatchInput(tc.matchID, tc.reason, tc.actor)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parseUnmatchInput error = %v, want %v", err, tc.wantErr)
			}
			if err := ValidateUnmatchInput(tc.matchID, tc.reason, tc.actor); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateUnmatchInput error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseUnmatchInputAcceptsValidShapes(t *testing.T) {
	req, err := parseUnmatchInput(strings.ToUpper(testMatchID),
		"  operator requested  ", "human:admin-1")
	if err != nil {
		t.Fatalf("parseUnmatchInput: %v", err)
	}
	if req.matchID != testMatchID {
		t.Fatalf("match id = %q, want canonical %q", req.matchID, testMatchID)
	}
	if req.reason != "operator requested" {
		t.Fatalf("reason = %q, want trimmed", req.reason)
	}
	if req.actor.String() != "human:admin-1" {
		t.Fatalf("actor = %q, want human:admin-1", req.actor.String())
	}

	// Exactly 500 bytes (including multibyte runes counted as bytes) is valid.
	exact := strings.Repeat("é", 250)
	if _, err := parseUnmatchInput(testMatchID, exact, testHumanID); err != nil {
		t.Fatalf("500-byte reason rejected: %v", err)
	}
}

func TestUnmatchRequiresCallerTransaction(t *testing.T) {
	service := &Service{}
	for _, tc := range []struct {
		name string
		tx   pgx.Tx
	}{
		{name: "nil", tx: nil},
		{name: "typed nil", tx: (*connTx)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.Unmatch(context.Background(), tc.tx, testMatchID,
				"operator requested", testHumanID)
			if !errors.Is(err, ErrNoTransaction) {
				t.Fatalf("Unmatch error = %v, want ErrNoTransaction", err)
			}
		})
	}
}
