package telemetry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var telemetryEnvKeys = []string{
	EnvSDKDisabled,
	EnvTracesExporter,
	EnvServiceName,
	EnvResourceAttributes,
	EnvEndpoint,
	EnvTracesEndpoint,
	EnvProtocol,
	EnvTracesProtocol,
	EnvHeaders,
	EnvTracesHeaders,
	EnvTracesSampler,
	EnvTracesSamplerArg,
	EnvCapturePayloads,
}

func clearTelemetryEnv(t *testing.T) {
	t.Helper()
	for _, key := range telemetryEnvKeys {
		t.Setenv(key, "")
	}
}

func TestConfigFromEnvDefaultsToNoop(t *testing.T) {
	clearTelemetryEnv(t)

	cfg := ConfigFromEnv("", "")
	if !cfg.Disabled {
		t.Fatal("ConfigFromEnv without an endpoint must be disabled")
	}
	if cfg.ServiceName != DefaultServiceName {
		t.Fatalf("ServiceName = %q, want %q", cfg.ServiceName, DefaultServiceName)
	}
	if cfg.Protocol != DefaultProtocol {
		t.Fatalf("Protocol = %q, want %q", cfg.Protocol, DefaultProtocol)
	}
	if cfg.Sampler != DefaultSampler {
		t.Fatalf("Sampler = %q, want %q", cfg.Sampler, DefaultSampler)
	}
	if cfg.SamplerArg != 1.0 {
		t.Fatalf("SamplerArg = %v, want 1.0", cfg.SamplerArg)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %s, want %s", cfg.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if cfg.CapturePayloads {
		t.Fatal("CapturePayloads must default to false")
	}
}

func TestConfigFromEnvDisableSwitches(t *testing.T) {
	t.Run("sdk disabled", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv(EnvEndpoint, "http://localhost:4318")
		t.Setenv(EnvSDKDisabled, "true")
		if cfg := ConfigFromEnv("", ""); !cfg.Disabled {
			t.Fatal("OTEL_SDK_DISABLED=true must disable tracing")
		}
	})

	t.Run("exporter none", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv(EnvEndpoint, "http://localhost:4318")
		t.Setenv(EnvTracesExporter, "NONE")
		if cfg := ConfigFromEnv("", ""); !cfg.Disabled {
			t.Fatal("OTEL_TRACES_EXPORTER=none must disable tracing")
		}
	})

	t.Run("endpoint enables", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv(EnvEndpoint, "http://localhost:4318")
		cfg := ConfigFromEnv("", "")
		if cfg.Disabled {
			t.Fatal("an endpoint must enable tracing")
		}
		if cfg.Endpoint != "http://localhost:4318" {
			t.Fatalf("Endpoint = %q", cfg.Endpoint)
		}
		if cfg.endpointIsSignal {
			t.Fatal("base endpoint must not be marked signal-specific")
		}
	})
}

func TestConfigFromEnvServiceNamePrecedence(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvServiceName, "env-agent")
	if cfg := ConfigFromEnv("named-agent", "v1.2.3"); cfg.ServiceName != "named-agent" {
		t.Fatalf("named service must win: got %q", cfg.ServiceName)
	} else if cfg.ServiceVersion != "v1.2.3" {
		t.Fatalf("ServiceVersion = %q", cfg.ServiceVersion)
	}
	if cfg := ConfigFromEnv("", ""); cfg.ServiceName != "env-agent" {
		t.Fatalf("OTEL_SERVICE_NAME must be used when no name is passed: got %q", cfg.ServiceName)
	}
}

func TestConfigFromEnvEndpointAndProtocolPrecedence(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvEndpoint, "http://base:4318")
	t.Setenv(EnvTracesEndpoint, "http://traces:4318/v1/traces")
	t.Setenv(EnvProtocol, "http/protobuf")
	t.Setenv(EnvTracesProtocol, "  GRPC ")

	cfg := ConfigFromEnv("", "")
	if cfg.Endpoint != "http://traces:4318/v1/traces" {
		t.Fatalf("Endpoint = %q, want traces-specific endpoint", cfg.Endpoint)
	}
	if !cfg.endpointIsSignal {
		t.Fatal("traces-specific endpoint must be marked signal-specific")
	}
	if cfg.Protocol != protocolGRPC {
		t.Fatalf("Protocol = %q, want grpc", cfg.Protocol)
	}
}

func TestConfigFromEnvHeaders(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvHeaders, "Authorization=Bearer%20base,X-Custom=a%2Cb,Empty=")
	t.Setenv(EnvTracesHeaders, "authorization=Bearer%20override")

	cfg := ConfigFromEnv("", "")
	if got := cfg.Headers["authorization"]; got != "Bearer override" {
		t.Fatalf("traces header override = %q", got)
	}
	if got := cfg.Headers["Authorization"]; got != "Bearer base" {
		t.Fatalf("base header = %q", got)
	}
	if got := cfg.Headers["X-Custom"]; got != "a,b" {
		t.Fatalf("escaped header = %q", got)
	}
	if got, ok := cfg.Headers["Empty"]; !ok || got != "" {
		t.Fatalf("empty header value = %q, %t", got, ok)
	}
	if _, ok := cfg.Headers[""]; ok {
		t.Fatal("empty header key must be ignored")
	}

	rendered := fmt.Sprint(cfg)
	if strings.Contains(rendered, "Bearer") || strings.Contains(rendered, "override") {
		t.Fatalf("Config string leaks header values: %s", rendered)
	}
	if !strings.Contains(rendered, "Headers:4 keys") {
		t.Fatalf("Config string = %s, want header count", rendered)
	}
}

func TestConfigFromEnvMalformedHeadersAreRejectedWhenEnabled(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvEndpoint, "http://localhost:4318")
	t.Setenv(EnvHeaders, "Authorization=ok,Malformed,=bad,Bad=%zz")

	cfg := ConfigFromEnv("", "")
	if !cfg.invalidHeaders {
		t.Fatal("ConfigFromEnv did not mark malformed headers")
	}
	if _, err := New(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New error = %v, want ErrInvalidConfig", err)
	}
}

func TestConfigFromEnvSamplerAndCapture(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvTracesSampler, " TraceIDRatio ")
	t.Setenv(EnvTracesSamplerArg, "0.25")
	t.Setenv(EnvCapturePayloads, "true")

	cfg := ConfigFromEnv("", "")
	if cfg.Sampler != samplerTraceIDRatio {
		t.Fatalf("Sampler = %q", cfg.Sampler)
	}
	if cfg.SamplerArg != 0.25 {
		t.Fatalf("SamplerArg = %v", cfg.SamplerArg)
	}
	if !cfg.CapturePayloads {
		t.Fatal("CapturePayloads = false, want true")
	}
	if !CapturePayloadsEnabled() {
		t.Fatal("CapturePayloadsEnabled = false, want true")
	}
}

func TestConfigFromEnvInvalidSamplerArgBecomesNaN(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvTracesSampler, "traceidratio")
	t.Setenv(EnvTracesSamplerArg, "not-a-number")

	cfg := ConfigFromEnv("", "")
	if !math.IsNaN(cfg.SamplerArg) {
		t.Fatalf("SamplerArg = %v, want NaN", cfg.SamplerArg)
	}
}

func TestNewRejectsInvalidConfigWhenEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "protocol",
			cfg:  Config{Endpoint: "http://localhost:4318", Protocol: "zipkin"},
		},
		{
			name: "sampler",
			cfg:  Config{Endpoint: "http://localhost:4318", Sampler: "sometimes"},
		},
		{
			name: "ratio too large",
			cfg: Config{
				Endpoint: "http://localhost:4318", Sampler: "traceidratio", SamplerArg: 1.5,
			},
		},
		{
			name: "ratio NaN",
			cfg: Config{
				Endpoint: "http://localhost:4318", Sampler: "parentbased_traceidratio", SamplerArg: math.NaN(),
			},
		},
		{
			name: "empty header key",
			cfg: Config{
				Endpoint: "http://localhost:4318", Headers: map[string]string{"": "value"},
			},
		},
		{
			name: "invalid http endpoint",
			cfg:  Config{Endpoint: "localhost:4318"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(context.Background(), test.cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewIgnoresInvalidConfigWhenDisabled(t *testing.T) {
	for _, cfg := range []Config{
		{Disabled: true, Protocol: "zipkin", Sampler: "sometimes"},
		{Protocol: "zipkin", Sampler: "sometimes"}, // no endpoint => no-op
	} {
		provider, err := New(context.Background(), cfg)
		if err != nil {
			t.Fatalf("New(%+v) error = %v", cfg, err)
		}
		if provider.Enabled() {
			t.Fatalf("New(%+v) enabled, want no-op", cfg)
		}
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}
}

func TestConfigFromEnvProducesUsableConfig(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv(EnvEndpoint, "http://localhost:4318")

	cfg := ConfigFromEnv("recon-agent", "test-version")
	if err := cfg.withDefaults().validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	sampler, err := cfg.withDefaults().sampler()
	if err != nil {
		t.Fatalf("sampler: %v", err)
	}
	if sampler == nil {
		t.Fatal("sampler is nil")
	}
}

func TestConfigShutdownTimeoutDefault(t *testing.T) {
	clearTelemetryEnv(t)
	cfg := ConfigFromEnv("", "")
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 5s", cfg.ShutdownTimeout)
	}
}
