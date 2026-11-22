package telemetry

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// MaxErrorTextBytes bounds the error text recorded on a span.
const MaxErrorTextBytes = 1 << 10

// StartSpan starts name on tracer and returns the span-bearing context. A nil
// tracer returns ctx unchanged plus an OTel no-op span; a nil ctx is treated
// as context.Background.
func StartSpan(ctx context.Context, tracer trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tracer == nil {
		return ctx, noop.Span{}
	}
	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// RecordError marks span failed and records redacted, bounded error text.
// The original error is never attached, so provider responses containing keys
// or headers cannot leak into the trace. It is safe on a nil or no-op span.
func RecordError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	message := RedactAndTruncate(err.Error(), MaxErrorTextBytes)
	span.RecordError(errors.New(message), trace.WithStackTrace(false))
	span.SetStatus(codes.Error, message)
}

// RedactAndTruncate removes secrets with the LLM redactor, then bounds the
// result to limit bytes at a valid UTF-8 boundary. It is used for span error
// text; SafePayload is the separate payload hook.
func RedactAndTruncate(text string, limit int) string {
	redacted := llm.Redact(text)
	if limit <= 0 || len(redacted) <= limit {
		return redacted
	}
	return truncateUTF8(redacted, limit)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	prefix := value[:limit]
	for len(prefix) > 0 {
		r, size := utf8.DecodeLastRuneInString(prefix)
		if r != utf8.RuneError || size > 1 {
			break
		}
		// The cut split a multi-byte rune, or the last byte is not valid
		// UTF-8 on its own. Drop it and reconsider the new boundary.
		prefix = prefix[:len(prefix)-1]
	}
	// Invalid bytes can also occur before a valid final rune (for example a
	// malformed payload with "a\xffb"). Dropping only trailing bytes would
	// leave them in the retained prefix, so remove every invalid sequence to
	// guarantee the documented valid-UTF-8 invariant.
	if !utf8.ValidString(prefix) {
		prefix = strings.ToValidUTF8(prefix, "")
	}
	return prefix
}
