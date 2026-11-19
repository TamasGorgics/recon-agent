package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// unopenedTx satisfies the pgx.Tx interface without a connection. The parse
// tests never execute SQL, so the embedded nil interface is never called.
type unopenedTx struct{ pgx.Tx }

// stubBeginner satisfies db.Beginner for constructor tests. Methods are never
// called by NewService.
type stubBeginner struct{ db.Beginner }

const (
	testCaseID  = "11111111-1111-4111-8111-111111111111"
	testOldID   = "22222222-2222-4222-8222-222222222222"
	testNewID   = "33333333-3333-4333-8333-333333333333"
	testHumanID = "human:alice"
)

func TestNewServiceValidatesDependencies(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))

	if _, err := NewService(Options{Clock: fake}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil DB error = %v, want ErrInvalidRequest", err)
	}
	var typedNilPool *pgxpool.Pool
	if _, err := NewService(Options{DB: typedNilPool, Clock: fake}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("typed-nil DB error = %v, want ErrInvalidRequest", err)
	}
	if _, err := NewService(Options{DB: stubBeginner{}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil clock error = %v, want ErrInvalidRequest", err)
	}
	var typedNilClock *clock.Fake
	if _, err := NewService(Options{DB: stubBeginner{}, Clock: typedNilClock}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("typed-nil clock error = %v, want ErrInvalidRequest", err)
	}
	if _, err := NewService(Options{DB: stubBeginner{}, Clock: fake}); err != nil {
		t.Fatalf("NewService: %v", err)
	}
}

func TestParseSupersedeRequestValidation(t *testing.T) {
	tests := []struct {
		name          string
		tx            pgx.Tx
		caseID        string
		newProposalID string
		actor         string
		reason        string
		wantErr       error
	}{
		{name: "nil transaction", tx: nil, caseID: testCaseID, actor: testHumanID,
			reason: ReasonLateMatch, wantErr: ErrNoTransaction},
		{name: "typed nil transaction", tx: (*connTx)(nil), caseID: testCaseID,
			actor: testHumanID, reason: ReasonLateMatch, wantErr: ErrNoTransaction},
		{name: "empty case id", tx: unopenedTx{}, actor: testHumanID,
			reason: ReasonLateMatch, wantErr: ErrInvalidRequest},
		{name: "malformed case id", tx: unopenedTx{}, caseID: "not-a-uuid",
			actor: testHumanID, reason: ReasonLateMatch, wantErr: ErrInvalidRequest},
		{name: "malformed new proposal id", tx: unopenedTx{}, caseID: testCaseID,
			newProposalID: "not-a-uuid", reason: ReasonNewProposal, wantErr: ErrInvalidRequest},
		{name: "malformed actor", tx: unopenedTx{}, caseID: testCaseID,
			actor: "alice", reason: ReasonLateMatch, wantErr: ErrInvalidActor},
		{name: "unknown actor class", tx: unopenedTx{}, caseID: testCaseID,
			actor: "robot:alice", reason: ReasonLateMatch, wantErr: ErrInvalidActor},
		{name: "agent actor", tx: unopenedTx{}, caseID: testCaseID,
			actor: "agent:model-v1", reason: ReasonLateMatch, wantErr: ErrActorNotAllowed},
		{name: "empty reason", tx: unopenedTx{}, caseID: testCaseID,
			actor: testHumanID, reason: "   ", wantErr: ErrInvalidReason},
		{name: "overlong reason", tx: unopenedTx{}, caseID: testCaseID,
			actor: testHumanID, reason: strings.Repeat("a", maxReasonRunes+1),
			wantErr: ErrInvalidReason},
		{name: "control character reason", tx: unopenedTx{}, caseID: testCaseID,
			actor: testHumanID, reason: "late\x00match", wantErr: ErrInvalidReason},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSupersedeRequest(context.Background(), tc.tx, tc.caseID,
				tc.newProposalID, tc.actor, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseSupersedeRequestAcceptsValidShapes(t *testing.T) {
	tests := []struct {
		name          string
		newProposalID string
		actor         string
		reason        string
	}{
		{name: "replacement by human", newProposalID: testNewID, actor: testHumanID,
			reason: ReasonNewProposal},
		{name: "cancel-only late match by system", actor: "system:matcher",
			reason: ReasonLateMatch},
		{name: "cancel-only unmatch by human", actor: "human:admin-1",
			reason: ReasonCaseReopened},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseSupersedeRequest(context.Background(), unopenedTx{},
				strings.ToUpper(testCaseID), tc.newProposalID, tc.actor, "  "+tc.reason+"  ")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if req.caseID != testCaseID {
				t.Fatalf("case id = %q, want lowercased %q", req.caseID, testCaseID)
			}
			if req.reason != tc.reason {
				t.Fatalf("reason = %q, want trimmed %q", req.reason, tc.reason)
			}
			if req.actor.String() != tc.actor {
				t.Fatalf("actor = %q, want %q", req.actor, tc.actor)
			}
			if req.newProposalID != strings.ToLower(tc.newProposalID) {
				t.Fatalf("new proposal id = %q, want %q", req.newProposalID,
					strings.ToLower(tc.newProposalID))
			}
		})
	}
}

// TestParseSupersedeRequestReasonShapes pins the shape-only reason contract:
// the three canonical spec 5.1 values are accepted on their corresponding
// path, while empty/blank, control-character, and oversized reasons are
// rejected before any database write.
func TestParseSupersedeRequestReasonShapes(t *testing.T) {
	accepted := []struct {
		name          string
		newProposalID string
		reason        string
	}{
		{name: "replacement canonical", newProposalID: testNewID,
			reason: ReasonNewProposal},
		{name: "late match canonical", reason: ReasonLateMatch},
		{name: "unmatch canonical", reason: ReasonCaseReopened},
	}
	for _, tc := range accepted {
		t.Run("accept "+tc.name, func(t *testing.T) {
			req, err := parseSupersedeRequest(context.Background(), unopenedTx{},
				testCaseID, tc.newProposalID, testHumanID, tc.reason)
			if err != nil {
				t.Fatalf("canonical reason %q rejected: %v", tc.reason, err)
			}
			if req.reason != tc.reason {
				t.Fatalf("reason = %q, want %q", req.reason, tc.reason)
			}
		})
	}

	rejected := []struct {
		name   string
		reason string
	}{
		{name: "empty", reason: ""},
		{name: "blank", reason: " \t "},
		{name: "control character", reason: "late\x00match"},
		{name: "oversized", reason: strings.Repeat("a", maxReasonRunes+1)},
	}
	for _, tc := range rejected {
		t.Run("reject "+tc.name, func(t *testing.T) {
			_, err := parseSupersedeRequest(context.Background(), unopenedTx{},
				testCaseID, "", testHumanID, tc.reason)
			if !errors.Is(err, ErrInvalidReason) {
				t.Fatalf("reason %q error = %v, want ErrInvalidReason", tc.reason, err)
			}
		})
	}
}

func TestParseSupersedeRequestCarriesTrace(t *testing.T) {
	ctx := telemetry.WithTraceID(context.Background(), "trace-wave-2")
	req, err := parseSupersedeRequest(ctx, unopenedTx{}, testCaseID, "",
		"system:matcher", ReasonLateMatch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.traceID == nil || *req.traceID != "trace-wave-2" {
		t.Fatalf("trace = %v, want trace-wave-2", req.traceID)
	}

	req, err = parseSupersedeRequest(context.Background(), unopenedTx{}, testCaseID, "",
		"system:matcher", ReasonLateMatch)
	if err != nil {
		t.Fatalf("parse without trace: %v", err)
	}
	if req.traceID != nil {
		t.Fatalf("trace = %v, want nil", *req.traceID)
	}
}

func TestCancelPayloadIsDeterministic(t *testing.T) {
	got, err := json.Marshal(cancelPayload{
		ProposalID: testOldID,
		CaseID:     testCaseID,
		Reason:     ReasonLateMatch,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"proposal_id":"` + testOldID + `","case_id":"` + testCaseID +
		`","reason":"` + ReasonLateMatch + `"}`
	if string(got) != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestCancelIdempotencyKey(t *testing.T) {
	if got, want := CancelIdempotencyKey(testOldID), "approval.cancel:"+testOldID; got != want {
		t.Fatalf("CancelIdempotencyKey = %q, want %q", got, want)
	}
}

// connTx is used only for a typed-nil pgx.Tx test case.
type connTx struct{ pgx.Tx }
