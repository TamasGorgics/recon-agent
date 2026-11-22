package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceIDPrefersActiveSpan(t *testing.T) {
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })

	spanCtx, span := tracerProvider.Tracer("test").Start(context.Background(), "case.run")
	defer span.End()
	spanTraceID := span.SpanContext().TraceID().String()
	if spanTraceID == "" {
		t.Fatal("span trace ID is empty")
	}

	ctx := WithTraceID(spanCtx, "trace-legacy")
	if got := TraceID(ctx); got != spanTraceID {
		t.Fatalf("TraceID = %q, want active span trace ID %q", got, spanTraceID)
	}
}

func TestTraceIDFallsBackToLegacyKey(t *testing.T) {
	ctx := WithTraceID(context.Background(), "trace-123")
	if got := TraceID(ctx); got != "trace-123" {
		t.Fatalf("TraceID = %q, want trace-123", got)
	}
}

func TestTraceIDEmptyWithoutSpanOrLegacy(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("TraceID(background) = %q, want empty", got)
	}
	var nilContext context.Context
	if got := TraceID(nilContext); got != "" {
		t.Fatalf("TraceID(nil) = %q, want empty", got)
	}
	invalid := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	if got := TraceID(invalid); got != "" {
		t.Fatalf("TraceID(invalid span context) = %q, want empty", got)
	}
}

func TestSpanTraceIDRequiresActiveSpan(t *testing.T) {
	if got := SpanTraceID(context.Background()); got != "" {
		t.Fatalf("SpanTraceID(background) = %q, want empty", got)
	}
	var nilContext context.Context
	if got := SpanTraceID(nilContext); got != "" {
		t.Fatalf("SpanTraceID(nil) = %q, want empty", got)
	}
	if got := SpanTraceID(WithTraceID(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); got != "" {
		t.Fatalf("SpanTraceID(legacy-only) = %q, want empty", got)
	}
	invalid := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	if got := SpanTraceID(invalid); got != "" {
		t.Fatalf("SpanTraceID(invalid span context) = %q, want empty", got)
	}

	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })
	spanCtx, span := tracerProvider.Tracer("test").Start(context.Background(), "case.run")
	defer span.End()
	want := span.SpanContext().TraceID().String()
	if got := SpanTraceID(WithTraceID(spanCtx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); got != want {
		t.Fatalf("SpanTraceID(active span with stale legacy key) = %q, want %q", got, want)
	}
}

func TestTraceIDFromContextLogsActiveSpan(t *testing.T) {
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })

	ctx, span := tracerProvider.Tracer("test").Start(context.Background(), "case.run")
	defer span.End()

	var buf bytes.Buffer
	FromContext(ctx, NewLogger(&buf, slog.LevelInfo)).Info("handled")
	records := decodeLines(t, &buf)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if got, want := records[0]["trace_id"], span.SpanContext().TraceID().String(); got != want {
		t.Fatalf("trace_id = %v, want %q", got, want)
	}
}

func TestValidTraceID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"00000000000000000000000000000000", true}, // regex contract, not OTel validity
		{"0123456789ABCDEF0123456789ABCDEF", false},
		{"0123456789abcdef0123456789abcde", false},
		{"0123456789abcdef0123456789abcdef0", false},
		{"trace-123", false},
		{"", false},
		{"g123456789abcdef0123456789abcdef", false},
	}
	for _, test := range tests {
		if got := ValidTraceID(test.id); got != test.want {
			t.Errorf("ValidTraceID(%q) = %t, want %t", test.id, got, test.want)
		}
	}
}
