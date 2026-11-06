package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// Finalize applies the M2-10 outcome policy to one finished run. The CLI, the
// gate helper, and Worker.process all call this function so a case becomes a
// proposal, an attempt, or a release exactly once.
//
//	proposal / escalated        no-op (the loop transitioned); release as a safety net
//	budget_exhausted / invalid  RecordFailedAttempt (attempt cap escalates agent_failed)
//	provider_error              Release; no attempt increment
//	ErrAgentDisabled            Release; no attempt increment
//	canceled / unexpected       Release; no attempt increment
//
// Every release and attempt is owner-guarded inside cases.Service. A lost lease
// is logged and no-op; it is never retried with a fresh version.
func Finalize(ctx context.Context, svc CaseService, workerID string, maxAttempts int, logger *slog.Logger, caseID string, result agent.Result, runErr error) {
	if logger == nil {
		logger = slog.Default()
	}
	logArgs := []any{
		"case_id", caseID,
		"run_id", result.RunID,
		"outcome", string(result.Outcome),
		"steps", result.Steps,
		"tool_calls", result.ToolCalls,
		"tokens_in", result.TokensIn,
		"tokens_out", result.TokensOut,
		"cost_usd", result.CostUSD.String(),
	}

	if runErr != nil {
		switch {
		case errors.Is(runErr, agent.ErrAgentDisabled):
			logger.Info("worker: run stopped by kill switch", logArgs...)
			release(svc, workerID, logger, caseID)
		case errors.Is(runErr, llm.ErrCassetteMiss):
			logger.Warn("worker: run missed its cassette", append(logArgs, "error", runErr)...)
			release(svc, workerID, logger, caseID)
		case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded), ctx.Err() != nil:
			logger.Info("worker: run canceled during shutdown", logArgs...)
			release(svc, workerID, logger, caseID)
		default:
			logger.Error("worker: run failed", append(logArgs, "error", runErr)...)
			release(svc, workerID, logger, caseID)
		}
		return
	}

	switch result.Outcome {
	case agent.OutcomeProposal, agent.OutcomeEscalated:
		logger.Info("worker: run finished", logArgs...)
		release(svc, workerID, logger, caseID)
	case agent.OutcomeBudgetExhausted, agent.OutcomeInvalidOutput:
		logger.Info("worker: run consumed an attempt", logArgs...)
		recordFailedAttempt(svc, workerID, maxAttempts, logger, caseID)
	case agent.OutcomeProviderError:
		logger.Warn("worker: run hit a provider error; no attempt", logArgs...)
		release(svc, workerID, logger, caseID)
	default:
		logger.Error("worker: run returned an unknown outcome", logArgs...)
		release(svc, workerID, logger, caseID)
	}
}

// release returns the lease without incrementing attempts. It always uses a
// fresh context so a canceled run context cannot prevent cleanup.
func release(svc CaseService, workerID string, logger *slog.Logger, caseID string) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if _, err := svc.Release(releaseCtx, caseID, workerID); err != nil {
		if lostLease(err) {
			logger.Info("worker: lease already lost; release no-op", "case_id", caseID, "error", err)
			return
		}
		logger.Warn("worker: release lease failed", "case_id", caseID, "error", err)
	}
}

// recordFailedAttempt atomically clears the lease and increments attempts; the
// same call escalates the case at the configured cap.
func recordFailedAttempt(svc CaseService, workerID string, maxAttempts int, logger *slog.Logger, caseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	updated, err := svc.RecordFailedAttempt(ctx, caseID, workerID, maxAttempts)
	if err != nil {
		if lostLease(err) {
			logger.Info("worker: lease already lost; attempt no-op", "case_id", caseID, "error", err)
			return
		}
		logger.Warn("worker: record failed attempt failed", "case_id", caseID, "error", err)
		return
	}
	logger.Info("worker: attempt recorded",
		"case_id", caseID, "attempts", updated.Attempts, "status", string(updated.Status))
}

// process runs one leased case through the shared finalization policy.
func (w *Worker) process(ctx context.Context, c cases.Case) {
	result, err := w.runner.Run(ctx, c.ID)
	w.recordSpend(result.CostUSD)
	Finalize(ctx, w.cases, w.id, w.config.MaxAttempts, w.logger, c.ID, result, err)
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

// lostLease reports the owner-guard sentinels of a lease that moved or expired.
func lostLease(err error) bool {
	return errors.Is(err, cases.ErrNotLeaseOwner) ||
		errors.Is(err, cases.ErrNotLeased) ||
		errors.Is(err, cases.ErrStale) ||
		errors.Is(err, cases.ErrLeaseExpired) ||
		errors.Is(err, cases.ErrCaseNotFound)
}
