package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		telemetry.EnvSDKDisabled,
		telemetry.EnvTracesExporter,
		telemetry.EnvServiceName,
		telemetry.EnvEndpoint,
		telemetry.EnvTracesEndpoint,
		telemetry.EnvProtocol,
		telemetry.EnvTracesProtocol,
		telemetry.EnvHeaders,
		telemetry.EnvTracesHeaders,
		telemetry.EnvTracesSampler,
		telemetry.EnvTracesSamplerArg,
		telemetry.EnvCapturePayloads,
	} {
		t.Setenv(key, "")
	}
}

func testLogger(out io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type recordingExporter struct {
	mu            sync.Mutex
	spans         []sdktrace.ReadOnlySpan
	shutdownCalls int
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
	return nil
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

func TestRunWithTelemetryDisabledIsNoop(t *testing.T) {
	clearOTelEnv(t)

	var received *telemetry.Provider
	err := runWithTelemetry(context.Background(), testLogger(io.Discard),
		func(ctx context.Context, provider *telemetry.Provider) error {
			received = provider
			if provider.Enabled() {
				t.Error("provider reported enabled without an OTLP endpoint")
			}
			_, span := provider.Tracer("test").Start(ctx, "noop.span")
			span.End()
			return nil
		})
	if err != nil {
		t.Fatalf("runWithTelemetry: %v", err)
	}
	if received == nil {
		t.Fatal("work did not receive a provider")
	}
	if err := received.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestRunWithTelemetryFlushesAfterWork(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(telemetry.EnvEndpoint, "http://localhost:4318")

	exporter := &recordingExporter{}
	var (
		received     *telemetry.Provider
		workReturned bool
	)
	err := runWithTelemetry(context.Background(), testLogger(io.Discard),
		func(ctx context.Context, provider *telemetry.Provider) error {
			received = provider
			_, span := provider.Tracer("test").Start(ctx, "wired.span")
			span.End()
			workReturned = true
			return nil
		},
		telemetry.WithExporter(exporter),
	)
	if err != nil {
		t.Fatalf("runWithTelemetry: %v", err)
	}
	if !workReturned || received == nil || !received.Enabled() {
		t.Fatalf("work/provider state = returned:%t provider:%v", workReturned, received)
	}
	spans := exporter.recorded()
	if len(spans) != 1 || spans[0].Name() != "wired.span" {
		t.Fatalf("exported spans = %+v, want one wired.span", spans)
	}
	if got := exporter.shutdownCount(); got != 1 {
		t.Fatalf("exporter Shutdown calls = %d, want 1", got)
	}
}

func TestRunWithTelemetryInvalidConfigFailsBeforeWork(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(telemetry.EnvEndpoint, "http://localhost:4318")
	t.Setenv(telemetry.EnvProtocol, "bogus")

	workCalled := false
	err := runWithTelemetry(context.Background(), testLogger(io.Discard),
		func(context.Context, *telemetry.Provider) error {
			workCalled = true
			return nil
		})
	if err == nil {
		t.Fatal("runWithTelemetry accepted an invalid OTLP protocol")
	}
	if !errors.Is(err, telemetry.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	if workCalled {
		t.Fatal("work ran despite a telemetry startup error")
	}
}
