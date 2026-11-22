package telemetry

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var noopTracerProvider = noop.NewTracerProvider()

// Provider owns an explicit OTel TracerProvider. It never mutates the OTel
// global provider, so parallel tests and multiple providers stay isolated.
// A nil *Provider is valid and behaves as a disabled provider.
type Provider struct {
	tracerProvider  *sdktrace.TracerProvider
	enabled         bool
	shutdownTimeout time.Duration
	capturePayloads bool

	shutdownOnce sync.Once
	shutdownErr  error
}

type options struct {
	exporter sdktrace.SpanExporter
	syncer   bool
}

// Option configures New for tests and embedding.
type Option func(*options)

// WithExporter injects a span exporter instead of constructing an OTLP one.
// Supplying an exporter enables tracing even when Config.Endpoint is empty,
// unless Config.Disabled is set.
func WithExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *options) {
		o.exporter = exporter
	}
}

// WithSyncer uses a simple span processor instead of the batched default so
// tests observe spans deterministically when they end.
func WithSyncer() Option {
	return func(o *options) {
		o.syncer = true
	}
}

// Init builds a Provider from cfg. It is New without test options and never
// touches the global OTel provider.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	return New(ctx, cfg)
}

// New builds a Provider. With no endpoint and no injected exporter, or with
// Disabled set, it returns a working no-op Provider with no exporter or
// processor goroutines.
func New(ctx context.Context, cfg Config, opts ...Option) (*Provider, error) {
	cfg = cfg.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}

	var newOpts options
	for _, opt := range opts {
		if opt != nil {
			opt(&newOpts)
		}
	}

	if cfg.Disabled {
		return newNoopProvider(cfg), nil
	}
	if newOpts.exporter == nil && strings.TrimSpace(cfg.Endpoint) == "" {
		return newNoopProvider(cfg), nil
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	sampler, err := cfg.sampler()
	if err != nil {
		return nil, err
	}

	exporter := newOpts.exporter
	if exporter == nil {
		exporter, err = newExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}

	var processor sdktrace.SpanProcessor
	if newOpts.syncer {
		processor = sdktrace.NewSimpleSpanProcessor(exporter)
	} else {
		processor = sdktrace.NewBatchSpanProcessor(exporter)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(processor),
	)
	return &Provider{
		tracerProvider:  tracerProvider,
		enabled:         true,
		shutdownTimeout: cfg.ShutdownTimeout,
		capturePayloads: cfg.CapturePayloads,
	}, nil
}

func newNoopProvider(cfg Config) *Provider {
	return &Provider{
		shutdownTimeout: cfg.ShutdownTimeout,
		capturePayloads: cfg.CapturePayloads,
	}
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String(AttrServiceName, cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String(AttrServiceVersion, cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String(AttrDeploymentEnvironment, cfg.Environment))
	}
	// Detectors merge last-wins, so explicit Config attributes win over
	// OTEL_RESOURCE_ATTRIBUTES while other environment attributes are kept.
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
}

func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Protocol {
	case protocolHTTPProtobuf:
		return newHTTPExporter(ctx, cfg)
	case protocolGRPC:
		return newGRPCExporter(ctx, cfg)
	default:
		return nil, fmt.Errorf("%w: unsupported OTLP protocol %q", ErrInvalidConfig, cfg.Protocol)
	}
}

func newHTTPExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	endpointURL, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || endpointURL.Scheme == "" || endpointURL.Host == "" {
		return nil, fmt.Errorf("%w: invalid OTLP/HTTP endpoint %q", ErrInvalidConfig, cfg.Endpoint)
	}
	if endpointURL.Scheme != "http" && endpointURL.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported OTLP/HTTP endpoint scheme %q", ErrInvalidConfig, endpointURL.Scheme)
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithHeaders(cfg.Headers),
	}
	if cfg.endpointIsSignal {
		// Signal-specific endpoints already name their path.
		exporterOpts = append(exporterOpts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	} else {
		exporterOpts = append(exporterOpts, otlptracehttp.WithEndpoint(endpointURL.Host))
		if path := strings.TrimRight(endpointURL.Path, "/"); path != "" {
			exporterOpts = append(exporterOpts, otlptracehttp.WithURLPath(path+"/v1/traces"))
		}
		if endpointURL.Scheme == "http" {
			exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
		}
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, exporterOpts...)
}

func newGRPCExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("%w: OTLP/gRPC endpoint is empty", ErrInvalidConfig)
	}

	host := endpoint
	insecure := cfg.Insecure
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("%w: invalid OTLP/gRPC endpoint %q", ErrInvalidConfig, endpoint)
		}
		switch parsed.Scheme {
		case "http":
			insecure = true
		case "https":
			// TLS is the default; an explicit Config.Insecure still wins.
		default:
			return nil, fmt.Errorf("%w: unsupported OTLP/gRPC endpoint scheme %q", ErrInvalidConfig, parsed.Scheme)
		}
		host = parsed.Host
	} else if loopbackHost(host) {
		// Plaintext is allowed for local development.
		insecure = true
	}

	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(host),
		otlptracegrpc.WithHeaders(cfg.Headers),
	}
	if insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, exporterOpts...)
}

func loopbackHost(endpoint string) bool {
	host := endpoint
	if splitHost, _, err := net.SplitHostPort(endpoint); err == nil {
		host = splitHost
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Enabled reports whether spans are exported by this provider.
func (p *Provider) Enabled() bool {
	return p != nil && p.enabled && p.tracerProvider != nil
}

// CapturePayloads reports whether callers may attach SafePayload snippets to
// spans. It is false for a nil or disabled provider.
func (p *Provider) CapturePayloads() bool {
	return p != nil && p.capturePayloads
}

// Tracer returns a tracer for scope. A nil or disabled provider returns the
// OTel no-op tracer, so callers never branch on "tracing enabled".
func (p *Provider) Tracer(scope string) trace.Tracer {
	if !p.Enabled() {
		return noopTracerProvider.Tracer(scope)
	}
	return p.tracerProvider.Tracer(scope)
}

// ForceFlush asks the span processors to export pending spans. It is safe on a
// nil or disabled provider.
func (p *Provider) ForceFlush(ctx context.Context) error {
	if p == nil || p.tracerProvider == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := p.boundedContext(ctx)
	defer cancel()
	return p.tracerProvider.ForceFlush(ctx)
}

// Shutdown flushes pending spans and releases exporter resources. It is
// idempotent and returns the first shutdown error. The caller context bounds
// the operation; when it has no deadline, Config.ShutdownTimeout applies.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tracerProvider == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		bounded, cancel := p.boundedContext(ctx)
		defer cancel()
		p.shutdownErr = p.tracerProvider.Shutdown(bounded)
	})
	return p.shutdownErr
}

func (p *Provider) boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || p.shutdownTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, p.shutdownTimeout)
}
