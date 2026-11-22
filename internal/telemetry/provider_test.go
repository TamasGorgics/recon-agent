package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/goleak"
)

type recordingExporter struct {
	mu            sync.Mutex
	spans         []sdktrace.ReadOnlySpan
	shutdownCalls int
	shutdownErr   error
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCalls++
	return e.shutdownErr
}

func (e *recordingExporter) recorded() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

func (e *recordingExporter) shutdownCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCalls
}

func spanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found in %d spans", name, len(spans))
	return nil
}

func attributeValue(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestInitWithInMemoryExporter(t *testing.T) {
	exporter := &recordingExporter{}
	provider, err := New(context.Background(),
		Config{ServiceName: "recon-agent", ServiceVersion: "v9.9.9", Environment: "test"},
		WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	})
	if !provider.Enabled() {
		t.Fatal("provider is disabled")
	}

	tracer := provider.Tracer("recon-agent/agent")
	ctx, root := StartSpan(context.Background(), tracer, "case.run",
		attribute.String(AttrCaseID, "case-1"),
		attribute.Int64(AttrTokensInput, 12),
	)
	if !root.SpanContext().IsValid() {
		t.Fatal("root span context is invalid")
	}
	rootSpanID := root.SpanContext().SpanID()
	rootTraceID := root.SpanContext().TraceID()

	_, child := StartSpan(ctx, tracer, "llm.call", attribute.String(AttrModel, "scripted"))
	if got := child.SpanContext().TraceID(); got != rootTraceID {
		t.Fatalf("child trace ID = %s, want %s", got, rootTraceID)
	}
	child.End()
	root.SetStatus(codes.Ok, "")
	root.End()

	spans := exporter.recorded()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}
	gotRoot := spanByName(t, spans, "case.run")
	gotChild := spanByName(t, spans, "llm.call")
	if got := gotRoot.Parent(); got.IsValid() {
		t.Fatalf("root parent = %s, want invalid", got.SpanID())
	}
	if got := gotChild.Parent().SpanID(); got != rootSpanID {
		t.Fatalf("persisted child parent = %s, want %s", got, rootSpanID)
	}
	if value, ok := attributeValue(gotRoot, AttrCaseID); !ok || value.AsString() != "case-1" {
		t.Fatalf("case id attribute = %v, %t", value, ok)
	}
	if value, ok := attributeValue(gotRoot, AttrTokensInput); !ok || value.AsInt64() != 12 {
		t.Fatalf("tokens attribute = %v, %t", value, ok)
	}
	if got := gotRoot.Status().Code; got != codes.Ok {
		t.Fatalf("root status = %v, want Ok", got)
	}

	resource := gotRoot.Resource()
	serviceName, ok := resource.Set().Value(AttrServiceName)
	if !ok || serviceName.AsString() != "recon-agent" {
		t.Fatalf("resource service.name = %v, %t", serviceName, ok)
	}
	if version, ok := resource.Set().Value(AttrServiceVersion); !ok || version.AsString() != "v9.9.9" {
		t.Fatalf("resource service.version = %v, %t", version, ok)
	}
	if environment, ok := resource.Set().Value(AttrDeploymentEnvironment); !ok || environment.AsString() != "test" {
		t.Fatalf("resource deployment.environment = %v, %t", environment, ok)
	}
}

func TestNoEndpointIsNoop(t *testing.T) {
	defer goleak.VerifyNone(t)

	provider, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if provider.Enabled() {
		t.Fatal("provider without an endpoint is enabled")
	}
	if provider.Tracer("recon-agent/agent") == nil {
		t.Fatal("no-op Tracer returned nil")
	}
	ctx, span := StartSpan(context.Background(), provider.Tracer("recon-agent/agent"), "noop")
	if ctx == nil || span == nil {
		t.Fatal("StartSpan returned nil")
	}
	if span.SpanContext().IsValid() {
		t.Fatal("no-op span has a valid span context")
	}
	span.End()
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestDisabledProviderWithExporterIsNoop(t *testing.T) {
	defer goleak.VerifyNone(t)

	exporter := &recordingExporter{}
	provider, err := New(context.Background(), Config{Disabled: true}, WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Enabled() {
		t.Fatal("Disabled provider is enabled")
	}
	_, span := StartSpan(context.Background(), provider.Tracer("scope"), "span")
	span.End()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(exporter.recorded()); got != 0 {
		t.Fatalf("disabled provider exported %d spans", got)
	}
	if got := exporter.shutdownCount(); got != 0 {
		t.Fatalf("disabled provider shut down the unused exporter %d times", got)
	}
}

func TestShutdownFlushesBatch(t *testing.T) {
	defer goleak.VerifyNone(t)

	exporter := &recordingExporter{}
	provider, err := New(context.Background(), Config{}, WithExporter(exporter))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tracer := provider.Tracer("recon-agent/agent")
	for index := 0; index < 5; index++ {
		_, span := StartSpan(context.Background(), tracer, "case.run")
		span.End()
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := len(exporter.recorded()); got != 5 {
		t.Fatalf("exported %d spans after Shutdown, want 5", got)
	}
	if got := exporter.shutdownCount(); got != 1 {
		t.Fatalf("exporter Shutdown calls = %d, want 1", got)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := exporter.shutdownCount(); got != 1 {
		t.Fatalf("second Shutdown reached exporter: %d calls", got)
	}
}

func TestShutdownReturnsFirstError(t *testing.T) {
	exporter := &recordingExporter{shutdownErr: errors.New("exporter unavailable")}
	provider, err := New(context.Background(), Config{}, WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := provider.Shutdown(context.Background())
	if first == nil {
		t.Fatal("Shutdown did not return the exporter error")
	}
	second := provider.Shutdown(context.Background())
	if !errors.Is(second, first) {
		t.Fatalf("second Shutdown = %v, want first error %v", second, first)
	}
}

func TestShutdownHonorsCallerContext(t *testing.T) {
	provider, err := New(context.Background(), Config{}, WithExporter(&recordingExporter{}), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = provider.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown with a canceled context returned nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown error = %v, want context.Canceled", err)
	}
}

func TestShutdownBoundedByConfigTimeout(t *testing.T) {
	exporter := &blockingShutdownExporter{unblock: make(chan struct{})}
	provider, err := New(context.Background(), Config{ShutdownTimeout: 20 * time.Millisecond},
		WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	err = provider.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown did not time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %s, want a bounded wait", elapsed)
	}
	close(exporter.unblock)
}

type blockingShutdownExporter struct {
	unblock chan struct{}
}

func (e *blockingShutdownExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e *blockingShutdownExporter) Shutdown(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.unblock:
		return nil
	}
}

func TestGlobalProviderIsNotMutated(t *testing.T) {
	before := otel.GetTracerProvider()
	provider, err := New(context.Background(), Config{}, WithExporter(&recordingExporter{}), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if after := otel.GetTracerProvider(); after != before {
		t.Fatal("New mutated the global OTel tracer provider")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if after := otel.GetTracerProvider(); after != before {
		t.Fatal("Shutdown mutated the global OTel tracer provider")
	}
}

func TestSamplers(t *testing.T) {
	t.Run("parent based always on samples root", func(t *testing.T) {
		exporter := &recordingExporter{}
		provider, err := New(context.Background(), Config{}, WithExporter(exporter), WithSyncer())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = provider.Shutdown(context.Background()) }()
		_, span := StartSpan(context.Background(), provider.Tracer("scope"), "root")
		if !span.SpanContext().IsSampled() {
			t.Fatal("root span was sampled away by the default sampler")
		}
		span.End()
		if len(exporter.recorded()) != 1 {
			t.Fatalf("exported %d spans, want 1", len(exporter.recorded()))
		}
	})

	t.Run("always off", func(t *testing.T) {
		exporter := &recordingExporter{}
		provider, err := New(context.Background(), Config{Sampler: "always_off"},
			WithExporter(exporter), WithSyncer())
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer func() { _ = provider.Shutdown(context.Background()) }()
		_, span := StartSpan(context.Background(), provider.Tracer("scope"), "root")
		if span.SpanContext().IsSampled() {
			t.Fatal("always_off span is sampled")
		}
		span.End()
		if len(exporter.recorded()) != 0 {
			t.Fatalf("always_off exported %d spans", len(exporter.recorded()))
		}
	})
}

func TestNilProviderIsSafe(t *testing.T) {
	var provider *Provider
	if provider.Enabled() {
		t.Fatal("nil provider is enabled")
	}
	if provider.CapturePayloads() {
		t.Fatal("nil provider captures payloads")
	}
	if provider.Tracer("scope") == nil {
		t.Fatal("nil provider Tracer returned nil")
	}
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("nil ForceFlush: %v", err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown: %v", err)
	}
}

func TestProviderCapturePayloads(t *testing.T) {
	provider, err := New(context.Background(), Config{CapturePayloads: true},
		WithExporter(&recordingExporter{}), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = provider.Shutdown(context.Background()) }()
	if !provider.CapturePayloads() {
		t.Fatal("CapturePayloads = false, want true")
	}
}

func TestResourceIncludesFromEnvAttributes(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvResourceAttributes, "deployment.environment=dev,recon.team=platform")

	exporter := &recordingExporter{}
	provider, err := New(context.Background(), Config{ServiceName: "recon-agent"},
		WithExporter(exporter), WithSyncer())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = provider.Shutdown(context.Background()) }()

	_, span := StartSpan(context.Background(), provider.Tracer("scope"), "case.run")
	span.End()
	spans := exporter.recorded()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	resource := spans[0].Resource()
	if value, ok := resource.Set().Value(AttrDeploymentEnvironment); !ok || value.AsString() != "dev" {
		t.Fatalf("deployment.environment = %v, %t", value, ok)
	}
	if value, ok := resource.Set().Value("recon.team"); !ok || value.AsString() != "platform" {
		t.Fatalf("recon.team = %v, %t", value, ok)
	}
	if value, ok := resource.Set().Value(AttrServiceName); !ok || value.AsString() != "recon-agent" {
		t.Fatalf("service.name = %v, %t", value, ok)
	}
}

func TestNewWithOTLPEndpointsConstructsCleanly(t *testing.T) {
	defer goleak.VerifyNone(t)

	tests := []Config{
		{Endpoint: "http://localhost:4318", Protocol: "http/protobuf"},
		{Endpoint: "http://localhost:4317", Protocol: "grpc"},
	}
	for _, cfg := range tests {
		provider, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatalf("New(%s): %v", cfg.Protocol, err)
		}
		if !provider.Enabled() {
			t.Fatalf("%s provider disabled", cfg.Protocol)
		}
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatalf("%s Shutdown: %v", cfg.Protocol, err)
		}
	}
}
