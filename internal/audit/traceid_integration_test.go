//go:build integration

package audit_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestAppendTraceIDFromContext proves the central fallback: an entry without an
// explicit trace ID inherits a valid active span context before hashing, an
// explicit caller value wins, and a non-traced append stays NULL. The audit
// chain verifies after all three rows.
func TestAppendTraceIDFromContext(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	exporter := tracetest.NewInMemoryExporter()
	provider, err := telemetry.New(ctx, telemetry.Config{ServiceName: "audit-test"},
		telemetry.WithExporter(exporter), telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	spanCtx, span := provider.Tracer("audit-test").Start(ctx, "audit.append")
	traceID := span.SpanContext().TraceID().String()

	fromContext := chainEntry(t, 1)
	fromContext.TraceID = nil
	appendCommitted(t, spanCtx, d.Pool, fromContext)
	span.End()

	explicit := chainEntry(t, 2)
	explicit.TraceID = ptr("trace-explicit")
	appendCommitted(t, spanCtx, d.Pool, explicit)

	nonTraced := chainEntry(t, 3)
	nonTraced.TraceID = nil
	appendCommitted(t, ctx, d.Pool, nonTraced)

	// A legacy WithTraceID value models the API middleware's correlation ID
	// derived from an unauthenticated inbound traceparent. It must never be
	// persisted as an audit trace ID because there is no active span.
	forged := chainEntry(t, 4)
	forged.TraceID = nil
	appendCommitted(t, telemetry.WithTraceID(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), d.Pool, forged)

	storedFromContext := readStoredRow(t, ctx, d.Pool, 1)
	if !storedFromContext.TraceID.Valid || storedFromContext.TraceID.String != traceID {
		t.Fatalf("context trace_id = %+v, want %q", storedFromContext.TraceID, traceID)
	}
	if !telemetry.ValidTraceID(storedFromContext.TraceID.String) {
		t.Fatalf("stored trace_id %q is not 32 lowercase hex", storedFromContext.TraceID.String)
	}

	storedExplicit := readStoredRow(t, ctx, d.Pool, 2)
	if !storedExplicit.TraceID.Valid || storedExplicit.TraceID.String != "trace-explicit" {
		t.Fatalf("explicit trace_id = %+v, want trace-explicit", storedExplicit.TraceID)
	}

	storedNonTraced := readStoredRow(t, ctx, d.Pool, 3)
	if storedNonTraced.TraceID.Valid {
		t.Fatalf("non-traced trace_id = %+v, want NULL", storedNonTraced.TraceID)
	}

	storedForged := readStoredRow(t, ctx, d.Pool, 4)
	if storedForged.TraceID.Valid {
		t.Fatalf("legacy-only trace_id = %+v, want NULL", storedForged.TraceID)
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("audit.Verify: %v", err)
	}
	if !report.OK() || report.Checked != 4 {
		t.Fatalf("audit.Verify checked=%d problems=%+v", report.Checked, report.Problems)
	}
}
