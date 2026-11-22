package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// startApplySpan starts executor.apply before the serializable apply
// transaction. The caller ends it after commit, rollback, replay, or the
// apply_failed transaction.
func (s *Service) startApplySpan(ctx context.Context, proposalID string) (context.Context, trace.Span) {
	return telemetry.StartSpan(ctx, s.tracer, "executor.apply",
		attribute.String(telemetry.AttrProposalID, proposalID),
		attribute.String(telemetry.AttrExecutorRunID, s.executorRunID))
}

// annotateApplyProposal adds the proposal-specific attributes once the locked
// row is available. It runs inside the apply transaction but performs no I/O:
// span attributes are in-memory until the span ends after the transaction.
func annotateApplyProposal(ctx context.Context, locked lockedProposal) {
	span := trace.SpanFromContext(ctx)
	attrs := make([]attribute.KeyValue, 0, 4)
	if locked.CaseID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrCaseID, locked.CaseID))
	}
	if locked.Type != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrProposalType, locked.Type))
	}
	if locked.IdempotencyKey != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrIdempotencyHash,
			idempotencyHash(locked.IdempotencyKey)))
	}
	if origin := applyOriginTraceID(ctx, locked.TraceID); origin != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrOriginTraceID, origin))
	}
	span.SetAttributes(attrs...)
}

// finalizeApplySpan records the terminal result and status. A recorded
// apply_failed is an Error span with the reason attribute; a returned error
// also records redacted error text. applied/replayed are Ok; no other status
// claims success.
func finalizeApplySpan(span trace.Span, result Result, err error) {
	if span == nil {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 3)
	if result.Status != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrApplyStatus, result.Status))
	}
	if result.LedgerTxnID != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrLedgerTxnID, result.LedgerTxnID))
	}
	if result.FailureReason != "" {
		attrs = append(attrs, attribute.String(telemetry.AttrFailureReason, result.FailureReason))
	}
	span.SetAttributes(attrs...)

	switch {
	case err != nil:
		telemetry.RecordError(span, err)
	case result.Status == StatusApplyFailed:
		span.SetStatus(codes.Error, result.FailureReason)
	case result.Status == StatusApplied || result.Status == StatusReplayed:
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// idempotencyHash is the lowercase SHA-256 of the proposal's idempotency key.
func idempotencyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// applyOriginTraceID returns the persisted proposal trace ID when it is valid
// and differs from the apply span's own trace. It never manufactures a parent.
func applyOriginTraceID(ctx context.Context, persisted string) string {
	if !telemetry.ValidTraceID(persisted) {
		return ""
	}
	if persisted == telemetry.TraceID(ctx) {
		return ""
	}
	return persisted
}
