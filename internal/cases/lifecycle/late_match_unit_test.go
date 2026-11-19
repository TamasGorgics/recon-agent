package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func TestReconcileLateMatchRequestValidation(t *testing.T) {
	service, err := NewService(Options{
		DB:    stubBeginner{},
		Clock: clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	tests := []struct {
		name    string
		tx      pgx.Tx
		matchID string
		actor   string
		wantErr error
	}{
		{name: "nil transaction", tx: nil, matchID: testCaseID,
			actor: "system:matcher", wantErr: ErrNoTransaction},
		{name: "typed nil transaction", tx: (*connTx)(nil), matchID: testCaseID,
			actor: "system:matcher", wantErr: ErrNoTransaction},
		{name: "empty match id", tx: unopenedTx{}, actor: "system:matcher",
			wantErr: ErrInvalidRequest},
		{name: "malformed match id", tx: unopenedTx{}, matchID: "not-a-uuid",
			actor: "system:matcher", wantErr: ErrInvalidRequest},
		{name: "malformed actor", tx: unopenedTx{}, matchID: testCaseID,
			actor: "matcher", wantErr: ErrInvalidActor},
		{name: "human actor", tx: unopenedTx{}, matchID: testCaseID,
			actor: "human:alice", wantErr: ErrActorNotAllowed},
		{name: "agent actor", tx: unopenedTx{}, matchID: testCaseID,
			actor: "agent:model-v1", wantErr: ErrActorNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.ReconcileLateMatch(context.Background(), tc.tx,
				tc.matchID, tc.actor)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestTerminalBreakStatus(t *testing.T) {
	tests := map[string]bool{
		breakStatusOpen:       false,
		breakStatusInCase:     false,
		breakStatusResolved:   true,
		breakStatusSuperseded: true,
		breakStatusVoid:       true,
	}
	for status, want := range tests {
		if got := terminalBreakStatus(status); got != want {
			t.Fatalf("terminalBreakStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestValidBreakStatus(t *testing.T) {
	for _, status := range []string{
		breakStatusOpen, breakStatusInCase, breakStatusResolved,
		breakStatusSuperseded, breakStatusVoid,
	} {
		if !validBreakStatus(status) {
			t.Fatalf("validBreakStatus(%q) = false", status)
		}
	}
	if validBreakStatus("unknown") {
		t.Fatal("validBreakStatus accepted an unknown status")
	}
}
