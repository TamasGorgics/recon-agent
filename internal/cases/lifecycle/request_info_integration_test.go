//go:build integration

// covers: FIN-24, FIN-30, FIN-33

package lifecycle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// requestInfo runs the production service in its own transaction, the way the
// approval decision hook does.
func (f *fixture) requestInfo(t *testing.T, ctx context.Context, proposalID string) error {
	t.Helper()
	return db.WithTx(ctx, f.app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return f.service.RequestInfo(ctx, tx, proposalID)
	})
}

// TestRequestInfoMovesProposalPendingCaseToWaitingInfo proves the happy path,
// the exact actor/reason/trace audit, no attempt increment, and idempotency.
func TestRequestInfoMovesProposalPendingCaseToWaitingInfo(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	if _, err := f.db.Pool.Exec(context.Background(),
		`UPDATE cases SET attempts = 2 WHERE id = $1::uuid`, caseID); err != nil {
		t.Fatalf("seed attempts: %v", err)
	}
	proposalID := f.seedProposal(t, caseID, "rejected")
	before := f.caseRow(t, caseID)
	if before.Attempts != 2 {
		t.Fatalf("seeded attempts = %d, want 2", before.Attempts)
	}

	ctx := telemetry.WithTraceID(context.Background(), "trace-request-info")
	if err := f.requestInfo(t, ctx, proposalID); err != nil {
		t.Fatalf("RequestInfo: %v", err)
	}

	after := f.caseRow(t, caseID)
	if after.Status != "waiting_info" || after.Version != before.Version+1 ||
		after.Attempts != before.Attempts {
		t.Fatalf("case = %s/version %d/attempts %d, want waiting_info/%d/%d",
			after.Status, after.Version, after.Attempts,
			before.Version+1, before.Attempts)
	}
	if stored := f.proposal(t, proposalID); stored.Status != "rejected" || stored.Version != 0 {
		t.Fatalf("proposal = %s/%d, want unchanged rejected/0",
			stored.Status, stored.Version)
	}

	transitions := f.caseTransitionAudits(t, caseID)
	if len(transitions) != 1 {
		t.Fatalf("case.transition audits = %d, want 1", len(transitions))
	}
	transition := transitions[0]
	if transition.ActorType != "system" || transition.ActorID != "case-lifecycle" {
		t.Fatalf("transition actor = %s:%s, want system:case-lifecycle",
			transition.ActorType, transition.ActorID)
	}
	afterPayload := jsonObject(t, transition.After)
	if afterPayload["status"] != "waiting_info" ||
		afterPayload["reason"] != caselifecycle.ReasonInfoRequested {
		t.Fatalf("transition after = %+v, want waiting_info/%s",
			afterPayload, caselifecycle.ReasonInfoRequested)
	}
	if transition.TraceID == nil || *transition.TraceID != "trace-request-info" {
		t.Fatalf("transition trace = %v, want trace-request-info", transition.TraceID)
	}
	if got := f.audits(t, caseID); len(got) != 1 {
		t.Fatalf("case audits = %d, want exactly one transition: %+v", len(got), got)
	}

	// A second call sees waiting_info and is a complete no-op.
	if err := f.requestInfo(t, ctx, proposalID); err != nil {
		t.Fatalf("repeated RequestInfo: %v", err)
	}
	if repeated := f.caseRow(t, caseID); !equalStoredCase(repeated, after) {
		t.Fatalf("repeated RequestInfo changed case: %+v -> %+v", after, repeated)
	}
	if got := len(f.caseTransitionAudits(t, caseID)); got != 1 {
		t.Fatalf("case.transition audits after repeat = %d, want 1", got)
	}
	if got := f.outbox(t); len(got) != 0 {
		t.Fatalf("RequestInfo wrote outbox rows: %+v", got)
	}
}

// TestRequestInfoWaitingInfoIsIdempotentNoOp covers a case already parked for
// information before the hook runs.
func TestRequestInfoWaitingInfoIsIdempotentNoOp(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "waiting_info", "")
	proposalID := f.seedProposal(t, caseID, "rejected")
	before := f.caseRow(t, caseID)

	if err := f.requestInfo(t, context.Background(), proposalID); err != nil {
		t.Fatalf("RequestInfo: %v", err)
	}
	if after := f.caseRow(t, caseID); !equalStoredCase(after, before) {
		t.Fatalf("waiting_info case changed: %+v -> %+v", before, after)
	}
	if got := f.audits(t, caseID); len(got) != 0 {
		t.Fatalf("case audits = %d, want 0", len(got))
	}
}

// TestRequestInfoTypedErrors covers every pre-write refusal without changing
// the proposal or case.
func TestRequestInfoTypedErrors(t *testing.T) {
	t.Run("proposal is not rejected", func(t *testing.T) {
		f := newFixture(t)
		caseID := f.seedCase(t, "proposal_pending", "")
		proposalID := f.seedProposal(t, caseID, "pending_approval")
		beforeCase := f.caseRow(t, caseID)

		err := f.requestInfo(t, context.Background(), proposalID)
		if !errors.Is(err, casestate.ErrIllegalTransition) {
			t.Fatalf("error = %v, want ErrIllegalTransition", err)
		}
		if after := f.caseRow(t, caseID); !equalStoredCase(after, beforeCase) {
			t.Fatalf("case changed: %+v -> %+v", beforeCase, after)
		}
		if got := f.audits(t, caseID); len(got) != 0 {
			t.Fatalf("case audits = %d, want 0", len(got))
		}
	})

	t.Run("proposal not found", func(t *testing.T) {
		f := newFixture(t)
		err := f.requestInfo(t, context.Background(),
			"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
		if !errors.Is(err, caselifecycle.ErrProposalNotFound) {
			t.Fatalf("error = %v, want ErrProposalNotFound", err)
		}
	})

	t.Run("malformed proposal id", func(t *testing.T) {
		f := newFixture(t)
		err := f.requestInfo(t, context.Background(), "not-a-uuid")
		if !errors.Is(err, caselifecycle.ErrInvalidRequest) {
			t.Fatalf("error = %v, want ErrInvalidRequest", err)
		}
	})

	t.Run("nil caller transaction", func(t *testing.T) {
		f := newFixture(t)
		caseID := f.seedCase(t, "proposal_pending", "")
		proposalID := f.seedProposal(t, caseID, "rejected")
		err := f.service.RequestInfo(context.Background(), nil, proposalID)
		if !errors.Is(err, caselifecycle.ErrNoTransaction) {
			t.Fatalf("error = %v, want ErrNoTransaction", err)
		}
	})

	for _, status := range []string{"open", "in_agent", "escalated"} {
		t.Run("case is "+status, func(t *testing.T) {
			f := newFixture(t)
			caseID := f.seedCase(t, status, "")
			proposalID := f.seedProposal(t, caseID, "rejected")
			beforeCase := f.caseRow(t, caseID)

			err := f.requestInfo(t, context.Background(), proposalID)
			if !errors.Is(err, casestate.ErrIllegalTransition) {
				t.Fatalf("error = %v, want ErrIllegalTransition", err)
			}
			if after := f.caseRow(t, caseID); !equalStoredCase(after, beforeCase) {
				t.Fatalf("case changed: %+v -> %+v", beforeCase, after)
			}
			if got := f.audits(t, caseID); len(got) != 0 {
				t.Fatalf("case audits = %d, want 0", len(got))
			}
		})
	}
}

// TestRequestInfoRollsBackWithCallerTransaction proves the method owns no
// transaction: a caller rollback removes the case transition and its audit.
func TestRequestInfoRollsBackWithCallerTransaction(t *testing.T) {
	f := newFixture(t)
	caseID := f.seedCase(t, "proposal_pending", "")
	proposalID := f.seedProposal(t, caseID, "rejected")
	before := f.caseRow(t, caseID)
	boom := errors.New("injected caller rollback")

	err := db.WithTx(context.Background(), f.app, pgx.TxOptions{},
		func(ctx context.Context, tx pgx.Tx) error {
			if err := f.service.RequestInfo(ctx, tx, proposalID); err != nil {
				return err
			}
			return boom
		})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the caller error", err)
	}
	if after := f.caseRow(t, caseID); !equalStoredCase(after, before) {
		t.Fatalf("case survived rollback: %+v -> %+v", before, after)
	}
	if got := f.audits(t, caseID); len(got) != 0 {
		t.Fatalf("case audits survived rollback: %+v", got)
	}
	if stored := f.proposal(t, proposalID); stored.Status != "rejected" || stored.Version != 0 {
		t.Fatalf("proposal changed: %+v", stored)
	}
}
