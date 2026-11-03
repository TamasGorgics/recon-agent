package telemetry

import (
	"context"
	"io"
	"log/slog"
)

type traceIDKey struct{}

// NewLogger returns a JSON slog logger that writes to w and drops records below
// level. Callers must not log secrets, raw tool arguments/results, or untrusted
// memos at info level.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}

// WithTraceID returns a context carrying id as the current trace identifier.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the trace identifier stored in ctx, or "" when absent.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}

// FromContext returns base annotated with trace_id when ctx carries a trace
// identifier. The base logger is returned unchanged when there is no trace ID.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	id := TraceID(ctx)
	if id == "" {
		return base
	}
	return base.With(slog.String("trace_id", id))
}
