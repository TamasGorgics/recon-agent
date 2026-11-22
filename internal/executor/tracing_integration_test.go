//go:build integration

package executor_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// TestExecutorApplyTracingInProcess proves executor.apply is a child of an
// active parent with the frozen attributes, and that the executor audit row
// carries the same trace ID.
func TestExecutorApplyTracingInProcess(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	exporter := tracetest.NewInMemoryExporter()
	provider := newExecutorTracingProvider(t, exporter)
	service := w.tracingService(t, provider)

	parentCtx, parent := provider.Tracer("executor-test").Start(context.Background(), "case.run")
	parentTraceID := parent.SpanContext().TraceID().String()
	parentSpanID := parent.SpanContext().SpanID()
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE proposals SET trace_id = $2 WHERE id = $1::uuid`,
		fixture.proposalID, parentTraceID); err != nil {
		t.Fatalf("set proposal trace_id: %v", err)
	}

	result, err := service.Apply(parentCtx, fixture.proposalID)
	parent.End()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("status = %s, want applied", result.Status)
	}
	if result.LedgerTxnID == "" {
		t.Fatal("applied adjustment has no ledger transaction id")
	}

	span := requireExecutorSpan(t, exporter.GetSpans(), "executor.apply")
	if span.SpanContext.TraceID().String() != parentTraceID {
		t.Fatalf("executor trace = %s, want parent %s", span.SpanContext.TraceID(), parentTraceID)
	}
	if span.Parent.SpanID() != parentSpanID {
		t.Fatalf("executor parent span = %s, want %s", span.Parent.SpanID(), parentSpanID)
	}
	if span.Status.Code != codes.Ok {
		t.Fatalf("executor status = %v, want Ok", span.Status.Code)
	}
	requireExecutorAttrString(t, span, telemetry.AttrProposalID, fixture.proposalID)
	requireExecutorAttrString(t, span, telemetry.AttrCaseID, fixture.caseID)
	requireExecutorAttrString(t, span, telemetry.AttrProposalType, "adjustment")
	requireExecutorAttrString(t, span, telemetry.AttrApplyStatus, executor.StatusApplied)
	requireExecutorAttrString(t, span, telemetry.AttrLedgerTxnID, result.LedgerTxnID)
	requireExecutorAttrString(t, span, telemetry.AttrExecutorRunID, "executor:test:run-1")
	requireExecutorAttrString(t, span, telemetry.AttrIdempotencyHash,
		executorIdempotencyHash("executor-fixture-"+fixture.proposalID))
	requireExecutorNoAttr(t, span, telemetry.AttrOriginTraceID)

	var auditTraceID *string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM audit_log
		  WHERE action = 'executor.applied' AND entity_id = $1`, fixture.proposalID).
		Scan(&auditTraceID); err != nil {
		t.Fatalf("read executor audit trace_id: %v", err)
	}
	if auditTraceID == nil || *auditTraceID != parentTraceID {
		t.Fatalf("executor audit trace_id = %v, want %s", auditTraceID, parentTraceID)
	}
}

// TestExecutorApplyTracingAsyncAndReplay proves an async apply starts a root
// span with recon.origin.trace_id, applied and replayed are Ok, and the failure
// path records Error + failure reason while still writing its audit row.
func TestExecutorApplyTracingAsyncAndReplay(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})
	exporter := tracetest.NewInMemoryExporter()
	provider := newExecutorTracingProvider(t, exporter)
	service := w.tracingService(t, provider)
	const origin = "fedcba9876543210fedcba9876543210"
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE proposals SET trace_id = $2 WHERE id = $1::uuid`, fixture.proposalID, origin); err != nil {
		t.Fatalf("set proposal trace_id: %v", err)
	}

	result, err := service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if result.Status != executor.StatusApplied {
		t.Fatalf("first status = %s, want applied", result.Status)
	}
	replay, err := service.Apply(context.Background(), fixture.proposalID)
	if err != nil {
		t.Fatalf("replay Apply: %v", err)
	}
	if replay.Status != executor.StatusReplayed {
		t.Fatalf("replay status = %s, want replayed", replay.Status)
	}

	spans := executorSpans(exporter.GetSpans(), "executor.apply")
	if len(spans) != 2 {
		t.Fatalf("executor.apply spans = %d, want 2", len(spans))
	}
	for _, span := range spans {
		if span.Parent.IsValid() {
			t.Fatalf("async executor span has parent %s, want root", span.Parent.SpanID())
		}
		requireExecutorAttrString(t, span, telemetry.AttrOriginTraceID, origin)
	}
	applied := executorSpanByStatus(t, spans, executor.StatusApplied)
	if applied.Status.Code != codes.Ok {
		t.Fatalf("applied status = %v, want Ok", applied.Status.Code)
	}
	replayed := executorSpanByStatus(t, spans, executor.StatusReplayed)
	if replayed.Status.Code != codes.Ok {
		t.Fatalf("replayed status = %v, want Ok", replayed.Status.Code)
	}

	var auditTraceID *string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM audit_log
		  WHERE action = 'executor.applied' AND entity_id = $1`, fixture.proposalID).
		Scan(&auditTraceID); err != nil {
		t.Fatalf("read executor audit trace_id: %v", err)
	}
	if auditTraceID == nil || *auditTraceID != applied.SpanContext.TraceID().String() {
		t.Fatalf("executor audit trace_id = %v, want %s", auditTraceID, applied.SpanContext.TraceID())
	}

	// A revalidation failure is a recorded apply_failed: Error status with the
	// machine reason and the transition audit row on the same trace.
	expired := w.newCase(t, caseSpec{expiresAt: timePtr(w.clock.Now().Add(-time.Hour))})
	if _, err := w.db.Pool.Exec(context.Background(),
		`UPDATE proposals SET trace_id = $2 WHERE id = $1::uuid`, expired.proposalID, origin); err != nil {
		t.Fatalf("set expired proposal trace_id: %v", err)
	}
	failed, err := service.Apply(context.Background(), expired.proposalID)
	if err != nil {
		t.Fatalf("expired Apply: %v", err)
	}
	if failed.Status != executor.StatusApplyFailed || failed.FailureReason != executor.FailureExpired {
		t.Fatalf("expired result = %+v, want apply_failed/expired", failed)
	}
	failedSpan := executorSpanByStatus(t, executorSpans(exporter.GetSpans(), "executor.apply"),
		executor.StatusApplyFailed)
	if failedSpan.Status.Code != codes.Error {
		t.Fatalf("apply_failed status = %v, want Error", failedSpan.Status.Code)
	}
	requireExecutorAttrString(t, failedSpan, telemetry.AttrFailureReason, executor.FailureExpired)
	requireExecutorAttrString(t, failedSpan, telemetry.AttrOriginTraceID, origin)

	var failureAuditTraceID *string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM audit_log
		  WHERE action = 'proposal.transition' AND entity_id = $1
		    AND after->>'status' = 'apply_failed'`, expired.proposalID).
		Scan(&failureAuditTraceID); err != nil {
		t.Fatalf("read apply_failed audit trace_id: %v", err)
	}
	if failureAuditTraceID == nil || *failureAuditTraceID != failedSpan.SpanContext.TraceID().String() {
		t.Fatalf("apply_failed audit trace_id = %v, want %s",
			failureAuditTraceID, failedSpan.SpanContext.TraceID())
	}
}

func (w *world) tracingService(t *testing.T, provider *telemetry.Provider) *executor.Service {
	t.Helper()
	opts := w.options()
	opts.Telemetry = provider
	service, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("executor.NewService with telemetry: %v", err)
	}
	return service
}

func newExecutorTracingProvider(t *testing.T, exporter *tracetest.InMemoryExporter) *telemetry.Provider {
	t.Helper()
	provider, err := telemetry.New(context.Background(), telemetry.Config{ServiceName: "executor-test"},
		telemetry.WithExporter(exporter), telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider
}

func timePtr(value time.Time) *time.Time { return &value }

func executorIdempotencyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func executorSpans(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	out := make(tracetest.SpanStubs, 0, len(spans))
	for _, span := range spans {
		if span.Name == name {
			out = append(out, span)
		}
	}
	return out
}

func requireExecutorSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	found := executorSpans(spans, name)
	if len(found) != 1 {
		t.Fatalf("%s spans = %d, want 1", name, len(found))
	}
	return found[0]
}

func executorSpanByStatus(t *testing.T, spans tracetest.SpanStubs, status string) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if value, ok := executorSpanAttr(span, telemetry.AttrApplyStatus); ok &&
			value.AsString() == status {
			return span
		}
	}
	t.Fatalf("executor span with status %q not found", status)
	return tracetest.SpanStub{}
}

func requireExecutorAttrString(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	value, ok := executorSpanAttr(span, key)
	if !ok {
		t.Fatalf("span %s missing attribute %s", span.Name, key)
	}
	if value.Type() != attribute.STRING || value.AsString() != want {
		t.Fatalf("span %s attribute %s = %v, want %q", span.Name, key, value.AsInterface(), want)
	}
}

func requireExecutorNoAttr(t *testing.T, span tracetest.SpanStub, key string) {
	t.Helper()
	if _, ok := executorSpanAttr(span, key); ok {
		t.Fatalf("span %s unexpectedly has attribute %s", span.Name, key)
	}
}

func executorSpanAttr(span tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
