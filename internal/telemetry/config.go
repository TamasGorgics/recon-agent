package telemetry

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Environment variables read by ConfigFromEnv. Standard OTel names are used
// for everything except the project-specific payload-capture opt-in.
const (
	EnvSDKDisabled           = "OTEL_SDK_DISABLED"
	EnvTracesExporter        = "OTEL_TRACES_EXPORTER"
	EnvServiceName           = "OTEL_SERVICE_NAME"
	EnvResourceAttributes    = "OTEL_RESOURCE_ATTRIBUTES"
	EnvEndpoint              = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvTracesEndpoint        = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvProtocol              = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvTracesProtocol        = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	EnvHeaders               = "OTEL_EXPORTER_OTLP_HEADERS"
	EnvTracesHeaders         = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	EnvTracesSampler         = "OTEL_TRACES_SAMPLER"
	EnvTracesSamplerArg      = "OTEL_TRACES_SAMPLER_ARG"
	EnvCapturePayloads       = "RECON_OTEL_CAPTURE_PAYLOADS"
	DefaultServiceName       = "recon-agent"
	DefaultProtocol          = "http/protobuf"
	DefaultSampler           = "parentbased_always_on"
	DefaultShutdownTimeout   = 5 * time.Second
	protocolGRPC             = "grpc"
	protocolHTTPProtobuf     = "http/protobuf"
	exporterNone             = "none"
	samplerAlwaysOn          = "always_on"
	samplerAlwaysOff         = "always_off"
	samplerTraceIDRatio      = "traceidratio"
	samplerParentBasedOn     = "parentbased_always_on"
	samplerParentBasedRatio  = "parentbased_traceidratio"
	defaultTraceIDRatioValue = 1.0
)

// ErrInvalidConfig marks configuration that was rejected at startup. Callers
// may use errors.Is to distinguish it from exporter construction failures.
var ErrInvalidConfig = errors.New("invalid telemetry configuration")

// Config controls provider construction. The zero value is a valid no-op
// configuration: New returns a disabled Provider when Endpoint is empty and no
// exporter is injected. Headers are never logged; Config.String redacts them.
type Config struct {
	ServiceName     string            // default "recon-agent"
	ServiceVersion  string            // build info / ldflags; may be empty
	Environment     string            // deployment.environment; may be empty
	Disabled        bool              // true disables export; ConfigFromEnv sets it when unset/off
	Endpoint        string            // OTLP base endpoint, e.g. http://localhost:4318
	Protocol        string            // "http/protobuf" (default) or "grpc"
	Headers         map[string]string // OTLP headers; never logged
	Sampler         string            // default "parentbased_always_on"
	SamplerArg      float64           // ratio for *traceidratio samplers
	ShutdownTimeout time.Duration     // default 5s
	CapturePayloads bool              // default false; see SafePayload
	Insecure        bool              // force plaintext OTLP; http:// and loopback default to plaintext

	// endpointIsSignal records whether Endpoint came from the signal-specific
	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT variable. HTTP exporter paths differ:
	// base endpoints gain "/v1/traces", signal endpoints are used as-is.
	endpointIsSignal bool
	// invalidHeaders records malformed header environment input. New rejects
	// it when tracing is enabled instead of exporting with partial headers.
	invalidHeaders bool
}

// String renders Config without header values so accidental logging cannot
// leak OTLP credentials.
func (c Config) String() string {
	keys := make([]string, 0, len(c.Headers))
	for key := range c.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fmt.Sprintf(
		"Config{ServiceName:%q, ServiceVersion:%q, Environment:%q, Disabled:%t, Endpoint:%q, Protocol:%q, Headers:%d keys %v, Sampler:%q, SamplerArg:%g, ShutdownTimeout:%s, CapturePayloads:%t}",
		c.ServiceName, c.ServiceVersion, c.Environment, c.Disabled, c.Endpoint,
		c.Protocol, len(keys), keys, c.Sampler, c.SamplerArg, c.ShutdownTimeout, c.CapturePayloads,
	)
}

// ConfigFromEnv builds Config from standard OTEL_* variables plus
// RECON_OTEL_CAPTURE_PAYLOADS. serviceName wins over OTEL_SERVICE_NAME when
// non-empty. No endpoint (base or traces-specific) produces a no-op config.
func ConfigFromEnv(serviceName, serviceVersion string) Config {
	cfg := Config{
		ServiceName:     strings.TrimSpace(serviceName),
		ServiceVersion:  serviceVersion,
		Protocol:        DefaultProtocol,
		Sampler:         DefaultSampler,
		SamplerArg:      defaultTraceIDRatioValue,
		ShutdownTimeout: DefaultShutdownTimeout,
		CapturePayloads: envTruthy(EnvCapturePayloads),
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = strings.TrimSpace(os.Getenv(EnvServiceName))
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}

	cfg.Endpoint = strings.TrimSpace(os.Getenv(EnvTracesEndpoint))
	if cfg.Endpoint != "" {
		cfg.endpointIsSignal = true
	} else {
		cfg.Endpoint = strings.TrimSpace(os.Getenv(EnvEndpoint))
	}

	if protocol := strings.TrimSpace(os.Getenv(EnvTracesProtocol)); protocol != "" {
		cfg.Protocol = normalizeProtocol(protocol)
	} else if protocol := strings.TrimSpace(os.Getenv(EnvProtocol)); protocol != "" {
		cfg.Protocol = normalizeProtocol(protocol)
	}

	baseHeaders, baseHeadersInvalid := parseHeadersEnv(os.Getenv(EnvHeaders))
	traceHeaders, traceHeadersInvalid := parseHeadersEnv(os.Getenv(EnvTracesHeaders))
	cfg.Headers = mergeHeaders(baseHeaders, traceHeaders)
	cfg.invalidHeaders = baseHeadersInvalid || traceHeadersInvalid

	if sampler := strings.TrimSpace(os.Getenv(EnvTracesSampler)); sampler != "" {
		cfg.Sampler = strings.ToLower(sampler)
	}
	if arg := strings.TrimSpace(os.Getenv(EnvTracesSamplerArg)); arg != "" {
		ratio, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			// NaN can never pass the sampler ratio validation in New. This
			// keeps ConfigFromEnv error-free while rejecting bad input at
			// startup instead of silently sampling everything.
			ratio = math.NaN()
		}
		cfg.SamplerArg = ratio
	}

	cfg.Disabled = envTruthy(EnvSDKDisabled) ||
		strings.EqualFold(strings.TrimSpace(os.Getenv(EnvTracesExporter)), exporterNone) ||
		cfg.Endpoint == ""
	return cfg
}

// CapturePayloadsEnabled reports the project payload-capture opt-in. It is
// false by default; instrumentation must use it before attaching SafePayload
// snippets to spans. It reads the environment on every call so tests can
// toggle it without rebuilding a Provider.
func CapturePayloadsEnabled() bool {
	return envTruthy(EnvCapturePayloads)
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.ServiceName) == "" {
		c.ServiceName = DefaultServiceName
	}
	if c.Protocol == "" {
		c.Protocol = DefaultProtocol
	} else {
		c.Protocol = normalizeProtocol(c.Protocol)
	}
	if c.Sampler == "" {
		c.Sampler = DefaultSampler
	} else {
		c.Sampler = strings.ToLower(strings.TrimSpace(c.Sampler))
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Headers == nil {
		c.Headers = map[string]string{}
	}
	return c
}

func (c Config) validate() error {
	switch c.Protocol {
	case protocolHTTPProtobuf, protocolGRPC:
	default:
		return fmt.Errorf("%w: unsupported OTLP protocol %q (want %q or %q)",
			ErrInvalidConfig, c.Protocol, protocolHTTPProtobuf, protocolGRPC)
	}
	for key := range c.Headers {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: OTLP header name must not be empty", ErrInvalidConfig)
		}
	}
	if c.invalidHeaders {
		return fmt.Errorf("%w: malformed OTLP headers", ErrInvalidConfig)
	}
	if _, err := c.sampler(); err != nil {
		return err
	}
	return nil
}

func (c Config) sampler() (sdktrace.Sampler, error) {
	switch c.Sampler {
	case samplerAlwaysOn:
		return sdktrace.AlwaysSample(), nil
	case samplerAlwaysOff:
		return sdktrace.NeverSample(), nil
	case samplerParentBasedOn:
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case samplerTraceIDRatio, samplerParentBasedRatio:
		if math.IsNaN(c.SamplerArg) || math.IsInf(c.SamplerArg, 0) {
			return nil, fmt.Errorf("%w: trace ID ratio must be a finite number", ErrInvalidConfig)
		}
		if c.SamplerArg < 0 || c.SamplerArg > 1 {
			return nil, fmt.Errorf("%w: trace ID ratio %g is outside [0,1]", ErrInvalidConfig, c.SamplerArg)
		}
		if c.Sampler == samplerParentBasedRatio {
			return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SamplerArg)), nil
		}
		return sdktrace.TraceIDRatioBased(c.SamplerArg), nil
	default:
		return nil, fmt.Errorf("%w: unsupported sampler %q", ErrInvalidConfig, c.Sampler)
	}
}

func normalizeProtocol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func envTruthy(key string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	return err == nil && value
}

// parseHeadersEnv parses the OTLP comma-separated "key=value" header format.
// Values are percent-decoded per the OTel environment specification. It
// reports whether any non-empty entry was malformed so New can reject it at
// startup. A valid entry with an empty value is retained.
func parseHeadersEnv(value string) (map[string]string, bool) {
	headers := map[string]string{}
	invalid := false
	for _, pair := range strings.Split(value, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		key, raw, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found {
			invalid = true
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			invalid = true
			continue
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(raw))
		if err != nil {
			invalid = true
			continue
		}
		headers[key] = decoded
	}
	return headers, invalid
}

func mergeHeaders(base, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		merged[key] = value
	}
	return merged
}
