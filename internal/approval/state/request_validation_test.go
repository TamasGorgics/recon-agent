package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

type stubBeginner struct{}

func (stubBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("stub beginner must not be used")
}

func (stubBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("stub beginner must not be used")
}

// stubTx satisfies pgx.Tx without a database. Validation-only tests never call
// a method on it.
type stubTx struct {
	pgx.Tx
}

func TestNewServiceValidation(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	if _, err := state.NewService(nil, fake); !errors.Is(err, state.ErrInvalidRequest) {
		t.Fatalf("NewService(nil db) error = %v, want ErrInvalidRequest", err)
	}
	if _, err := state.NewService(stubBeginner{}, nil); !errors.Is(err, state.ErrInvalidRequest) {
		t.Fatalf("NewService(nil clock) error = %v, want ErrInvalidRequest", err)
	}
	if _, err := state.NewService(stubBeginner{}, fake); err != nil {
		t.Fatalf("NewService(valid) error = %v", err)
	}
}

func TestTransitionRequestValidation(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	service, err := state.NewService(stubBeginner{}, fake)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	system, err := audit.SystemActor("test")
	if err != nil {
		t.Fatalf("SystemActor: %v", err)
	}
	trace := "trace"
	var tx pgx.Tx = (*stubTx)(nil)

	base := state.TransitionRequest{
		ProposalID:      "00000000-0000-4000-8000-000000000001",
		To:              state.StatusPendingApproval,
		ExpectedVersion: 0,
		Actor:           system,
		TraceID:         &trace,
	}

	tests := []struct {
		name   string
		mutate func(*state.TransitionRequest)
		tx     pgx.Tx
		want   error
	}{
		{
			name:   "empty proposal id",
			mutate: func(req *state.TransitionRequest) { req.ProposalID = "" },
			tx:     tx,
			want:   state.ErrInvalidRequest,
		},
		{
			name:   "negative expected version",
			mutate: func(req *state.TransitionRequest) { req.ExpectedVersion = -1 },
			tx:     tx,
			want:   state.ErrInvalidRequest,
		},
		{
			name:   "nil transaction",
			mutate: func(*state.TransitionRequest) {},
			tx:     nil,
			want:   state.ErrInvalidRequest,
		},
		{
			name:   "unknown target status",
			mutate: func(req *state.TransitionRequest) { req.To = "escalated" },
			tx:     tx,
			want:   state.ErrUnknownStatus,
		},
		{
			name: "invalid actor",
			mutate: func(req *state.TransitionRequest) {
				req.Actor = audit.Actor{}
			},
			tx:   tx,
			want: state.ErrInvalidActor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			_, err := service.Transition(context.Background(), tc.tx, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Transition error = %v, want %v", err, tc.want)
			}
			if tc.want == state.ErrInvalidActor && !errors.Is(err, audit.ErrInvalidActor) {
				t.Fatalf("invalid actor error = %v, want it to wrap audit.ErrInvalidActor", err)
			}
		})
	}

	// Validation precedence: an unknown target wins over an invalid actor.
	req := base
	req.To = "escalated"
	req.Actor = audit.Actor{}
	if _, err := service.Transition(context.Background(), tx, req); !errors.Is(err, state.ErrUnknownStatus) {
		t.Fatalf("unknown status precedence error = %v, want ErrUnknownStatus", err)
	}
}
