package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func TestParseWaitingRequestValidation(t *testing.T) {
	service, err := NewService(Options{
		DB:    stubBeginner{},
		Clock: clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	tests := []struct {
		name   string
		tx     any
		caseID string
		actor  string
		reason string
		want   error
	}{
		{name: "nil transaction", caseID: testCaseID, actor: "human:alice",
			reason: ReasonHumanReply, want: ErrNoTransaction},
		{name: "typed nil transaction", tx: (*connTx)(nil), caseID: testCaseID,
			actor: "human:alice", reason: ReasonHumanReply, want: ErrNoTransaction},
		{name: "malformed case id", tx: unopenedTx{}, caseID: "not-a-uuid",
			actor: "human:alice", reason: ReasonHumanReply, want: ErrInvalidRequest},
		{name: "malformed actor", tx: unopenedTx{}, caseID: testCaseID,
			actor: "alice", reason: ReasonHumanReply, want: ErrInvalidActor},
		{name: "agent actor", tx: unopenedTx{}, caseID: testCaseID,
			actor: "agent:model-v1", reason: ReasonHumanReply, want: ErrActorNotAllowed},
		{name: "unknown reason", tx: unopenedTx{}, caseID: testCaseID,
			actor: "human:alice", reason: "because", want: ErrInvalidWaitingReason},
		{name: "empty reason", tx: unopenedTx{}, caseID: testCaseID,
			actor: "human:alice", reason: "  ", want: ErrInvalidWaitingReason},
		{name: "human reply by system", tx: unopenedTx{}, caseID: testCaseID,
			actor: "system:ingest", reason: ReasonHumanReply, want: ErrInvalidWaitingReason},
		{name: "new ingest by human", tx: unopenedTx{}, caseID: testCaseID,
			actor: "human:alice", reason: ReasonNewIngest, want: ErrInvalidWaitingReason},
		{name: "new ingest by wrong system", tx: unopenedTx{}, caseID: testCaseID,
			actor: "system:matcher", reason: ReasonNewIngest, want: ErrInvalidWaitingReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tx pgx.Tx
			switch typed := tc.tx.(type) {
			case nil:
				tx = nil
			case *connTx:
				tx = typed
			case unopenedTx:
				tx = typed
			}
			_, err := service.ResumeWaiting(context.Background(), tx, tc.caseID,
				tc.actor, tc.reason)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseWaitingRequestAcceptsBothPairs(t *testing.T) {
	tests := []struct {
		name   string
		actor  string
		reason string
	}{
		{name: "human reply", actor: "human:alice", reason: ReasonHumanReply},
		{name: "new ingest", actor: IngestActor, reason: ReasonNewIngest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request, err := parseWaitingRequest(unopenedTx{}, testCaseID,
				tc.actor, " "+tc.reason+" ")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if request.actor.String() != tc.actor || request.reason != tc.reason {
				t.Fatalf("request = %s/%s, want %s/%s", request.actor,
					request.reason, tc.actor, tc.reason)
			}
		})
	}
}

func TestOnIngestTouchedCaseValidation(t *testing.T) {
	service, err := NewService(Options{
		DB:    stubBeginner{},
		Clock: clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := service.OnIngestTouchedCase(context.Background(), nil, testCaseID); !errors.Is(err, ErrNoTransaction) {
		t.Fatalf("nil tx error = %v, want ErrNoTransaction", err)
	}
	if err := service.OnIngestTouchedCase(context.Background(), unopenedTx{}, "not-a-uuid"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed case error = %v, want ErrInvalidRequest", err)
	}
}
