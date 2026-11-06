package worker

import (
	"context"
	"errors"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// process runs one leased case and applies the M2-10 finalization policy:
//
//	proposal / escalated        no-op (the loop transitioned); release as a safety net
//	budget_exhausted / invalid  RecordFailedAttempt (attempt cap escalates agent_failed)
//	provider_error              Release; no attempt increment
//	ErrAgentDisabled            Release; no attempt increment
//	canceled / unexpected       Release; no attempt increment
//
// Every release and attempt is owner-guarded inside cases.Service. A lost lease
// is logged and no-op; it is never retried with a fresh version.
func (w *Worker) process(ctx context.Context, c cases.Case) {
	result, err := w.runner.Run(ctx, c.ID)
	w.recordSpend(result.CostUSD)

	logArgs := []any{
		"case_id", c.ID,
		"run_id", result.RunID,
		"outcome", string(result.Outcome),
		"steps", result.Steps,
		"tool_calls", result.ToolCalls,
		"tokens_in", result.TokensIn,
		"tokens_out", result.TokensOut,
		"cost_usd", result.CostUSD.String(),
	}

	if err != nil {
		switch {
		case errors.Is(err, agent.ErrAgentDisabled):
			w.logger.Info("worker: run stopped by kill switch", logArgs...)
			w.release(c.ID)
		case errors.Is(err, llm.ErrCassetteMiss):
			w.logger.Warn("worker: run missed its cassette", append(logArgs, "error", err)...)
			w.release(c.ID)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
			w.logger.Info("worker: run canceled during shutdown", logArgs...)
			w.release(c.ID)
		default:
			w.logger.Error("worker: run failed", append(logArgs, "error", err)...)
			w.release(c.ID)
		}
		return
	}

	switch result.Outcome {
	case agent.OutcomeProposal, agent.OutcomeEscalated:
		w.logger.Info("worker: run finished", logArgs...)
		w.release(c.ID)
	case agent.OutcomeBudgetExhausted, agent.OutcomeInvalidOutput:
		w.logger.Info("worker: run consumed an attempt", logArgs...)
		w.recordFailedAttempt(c.ID)
	case agent.OutcomeProviderError:
		w.logger.Warn("worker: run hit a provider error; no attempt", logArgs...)
		w.release(c.ID)
	default:
		w.logger.Error("worker: run returned an unknown outcome", logArgs...)
		w.release(c.ID)
	}
}

// escalateExhausted dead-letters a case that reached the attempt cap without a
// model call. A lost lease is a no-op.
func (w *Worker) escalateExhausted(ctx context.Context, c cases.Case) error {
	_, err := w.cases.Transition(ctx, cases.TransitionRequest{
		CaseID:           c.ID,
		ExpectedVersion:  c.Version,
		Status:           cases.StatusEscalated,
		EscalationReason: agentFailedReason,
	})
	if err != nil {
		if lostLease(err) {
			w.logger.Info("worker: attempt-cap escalation lost the lease; no-op",
				"case_id", c.ID, "error", err)
			return nil
		}
		w.logger.Warn("worker: attempt-cap escalation failed", "case_id", c.ID, "error", err)
		return err
	}
	w.logger.Info("worker: case escalated at attempt cap",
		"case_id", c.ID, "attempts", c.Attempts, "reason", agentFailedReason)
	return nil
}

// release returns the lease without incrementing attempts. It always uses a
// fresh context so a canceled run context cannot prevent cleanup.
func (w *Worker) release(caseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if _, err := w.cases.Release(ctx, caseID, w.id); err != nil {
		if lostLease(err) {
			w.logger.Info("worker: lease already lost; release no-op", "case_id", caseID, "error", err)
			return
		}
		w.logger.Warn("worker: release lease failed", "case_id", caseID, "error", err)
	}
}

// recordFailedAttempt atomically clears the lease and increments attempts; the
// same call escalates the case at the configured cap.
func (w *Worker) recordFailedAttempt(caseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	updated, err := w.cases.RecordFailedAttempt(ctx, caseID, w.id, w.config.MaxAttempts)
	if err != nil {
		if lostLease(err) {
			w.logger.Info("worker: lease already lost; attempt no-op", "case_id", caseID, "error", err)
			return
		}
		w.logger.Warn("worker: record failed attempt failed", "case_id", caseID, "error", err)
		return
	}
	w.logger.Info("worker: attempt recorded",
		"case_id", caseID, "attempts", updated.Attempts, "status", string(updated.Status))
}

// lostLease reports the owner-guard sentinels of a lease that moved or expired.
func lostLease(err error) bool {
	return errors.Is(err, cases.ErrNotLeaseOwner) ||
		errors.Is(err, cases.ErrNotLeased) ||
		errors.Is(err, cases.ErrStale) ||
		errors.Is(err, cases.ErrLeaseExpired) ||
		errors.Is(err, cases.ErrCaseNotFound)
}
