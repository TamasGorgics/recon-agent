package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

type traceIDKey struct{}

// WithTraceID returns a context carrying id as the legacy trace identifier.
// Active OTel spans take precedence in TraceID, so this remains a fallback for
// callers without spans (API middleware, Slack, tests).
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the active OTel trace ID when ctx carries a valid span
// context, otherwise the legacy WithTraceID value, otherwise "". Nil contexts
// return "". It is for logs and HTTP correlation (including X-Trace-ID), where
// a caller-supplied legacy value is useful; persistence paths must use
// SpanTraceID instead so an inbound traceparent can never become a stored
// trace ID.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		return spanContext.TraceID().String()
	}
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}

// SpanTraceID returns the trace ID of the valid active OTel span in ctx, or ""
// when there is no span. It never falls back to WithTraceID. Persistence paths
// (agent_runs.trace_id, proposals.trace_id, and audit.Append's fallback) use it
// so a legacy API correlation ID, including an unauthenticated inbound
// traceparent, cannot be forged into a database trace ID (spec A6 boundary).
func SpanTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	id := spanContext.TraceID().String()
	if !ValidTraceID(id) {
		return ""
	}
	return id
}

// ValidTraceID reports whether id is exactly 32 lowercase hexadecimal
// characters. Persistence paths use it so legacy/test strings such as
// "trace-123" never become database trace IDs.
func ValidTraceID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		character := id[i]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
