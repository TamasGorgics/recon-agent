package lifecycle

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// testSpanTraceID is the active OTel trace ID used by lifecycle tests that
// assert the audit trace is populated. Persistence must never read the legacy
// WithTraceID correlation value, so tests carry a real span context instead.
const testSpanTraceID = "0123456789abcdef0123456789abcdef"

// spanTraceContext returns a context carrying a valid, non-recording OTel span
// context with testSpanTraceID.
func spanTraceContext(t *testing.T) context.Context {
	t.Helper()
	traceID, err := trace.TraceIDFromHex(testSpanTraceID)
	if err != nil {
		t.Fatalf("parse trace ID: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("1111111111111111")
	if err != nil {
		t.Fatalf("parse span ID: %v", err)
	}
	return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	}))
}
