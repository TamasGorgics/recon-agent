package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

func TestStartSpanNilTracerIsNoop(t *testing.T) {
	type contextKey struct{}
	original := context.WithValue(context.Background(), contextKey{}, "value")

	ctx, span := StartSpan(original, nil, "case.run", attribute.String(AttrCaseID, "case-1"))
	if ctx != original {
		t.Fatal("StartSpan with a nil tracer replaced the context")
	}
	if span == nil {
		t.Fatal("StartSpan returned a nil span")
	}
	if span.SpanContext().IsValid() {
		t.Fatal("no-op span has a valid span context")
	}
	span.SetStatus(codes.Error, "ignored")
	span.RecordError(errors.New("ignored"))
	span.End()
}

func TestStartSpanNilContext(t *testing.T) {
	var nilContext context.Context
	_, span := StartSpan(nilContext, nil, "case.run")
	if span == nil {
		t.Fatal("StartSpan returned a nil span")
	}
	span.End()
}

func TestRecordErrorRedactsAndTruncates(t *testing.T) {
	exporter := &recordingExporter{}
	provider, err := New(context.Background(), Config{}, WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = provider.Shutdown(context.Background()) }()

	_, span := StartSpan(context.Background(), provider.Tracer("scope"), "llm.call")
	secretError := errors.New("Authorization: Bearer verysecret oc_sk_abc123 " + strings.Repeat("detail ", 400))
	RecordError(span, secretError)
	span.End()

	spans := exporter.recorded()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	recorded := spans[0]
	if got := recorded.Status().Code; got != codes.Error {
		t.Fatalf("status = %v, want Error", got)
	}
	description := recorded.Status().Description
	if strings.Contains(description, "verysecret") || strings.Contains(description, "oc_sk_abc123") {
		t.Fatalf("status description leaked a secret: %q", description)
	}
	if !strings.Contains(description, "[REDACTED]") {
		t.Fatalf("status description was not redacted: %q", description)
	}
	if len(description) > MaxErrorTextBytes {
		t.Fatalf("status description has %d bytes, want <= %d", len(description), MaxErrorTextBytes)
	}
	events := recorded.Events()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	for _, attr := range events[0].Attributes {
		if strings.Contains(attr.Value.String(), "verysecret") || strings.Contains(attr.Value.String(), "oc_sk_abc123") {
			t.Fatalf("event attribute %q leaked a secret: %q", attr.Key, attr.Value.String())
		}
	}
}

func TestRecordErrorSafeOnNilAndNoop(t *testing.T) {
	RecordError(nil, errors.New("ignored"))
	_, span := StartSpan(context.Background(), nil, "noop")
	RecordError(span, errors.New("ignored"))
	span.End()
}

func TestRedactAndTruncate(t *testing.T) {
	got := RedactAndTruncate("Bearer secret-value", 4)
	if got != "[RED" {
		t.Fatalf("RedactAndTruncate = %q, want %q", got, "[RED")
	}
	if got := RedactAndTruncate(strings.Repeat("é", 10), 5); got != "éé" {
		t.Fatalf("UTF-8 truncation = %q, want %q", got, "éé")
	}
}
