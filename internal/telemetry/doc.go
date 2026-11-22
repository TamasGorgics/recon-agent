// Package telemetry provides trace-correlated structured logging, an explicit
// OpenTelemetry provider, span helpers, and the single approved redaction hook
// for bounded payload snippets.
//
// Tracing is configured with standard OTLP environment variables (see
// ConfigFromEnv). With no endpoint, OTEL_SDK_DISABLED=true, or
// OTEL_TRACES_EXPORTER=none, New returns a working no-op Provider: no exporter
// or processor goroutines are started and Shutdown is safe. The OTel global
// provider is never mutated; applications receive a *Provider or trace.Tracer
// through explicit injection.
//
// TraceID is the log/HTTP correlation helper and may return a caller-supplied
// legacy value; persistence paths must use SpanTraceID, which only returns the
// active OTel span trace ID.
//
// Span payload capture is off by default. Only when
// RECON_OTEL_CAPTURE_PAYLOADS=true may instrumentation attach
// SafePayloadAttribute snippets; otherwise spans carry hashes, counts, IDs,
// codes, and cost only. SafePayload redacts with internal/llm before
// truncating and never includes system prompts, memos, or raw tool arguments.
package telemetry
