package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Bounded listener defaults. The binaries pass the validated config values;
// these apply only when a caller leaves ServerOptions zero.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultShutdownTimeout   = 5 * time.Second
)

// Handler serves one gatherer as Prometheus text. It gathers once per scrape.
// A gather error with zero families returns 500 with a fixed body so collector
// error text (which may contain infrastructure detail) never reaches the
// response. A partial gather error keeps the reachable families and returns
// 200, matching promhttp.ContinueOnError; the error is logged.
func Handler(g prometheus.Gatherer, logger *slog.Logger) http.Handler {
	if g == nil {
		g = prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
			return nil, errors.New("metrics: nil gatherer")
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		families, gatherErr := g.Gather()
		if gatherErr != nil && len(families) == 0 {
			if logger != nil {
				logger.Error("metrics gather failed", "error", gatherErr)
			}
			http.Error(w, "metrics: gather failed", http.StatusInternalServerError)
			return
		}

		// Hand the already-gathered snapshot to promhttp so the scrape does
		// exactly one gather (W4 collectors query the database per gather).
		snapshot := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
			return families, gatherErr
		})
		opts := promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}
		if logger != nil {
			opts.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
		}
		promhttp.HandlerFor(snapshot, opts).ServeHTTP(w, r)
	})
}

// Handler returns the HTTP handler for this process registry.
func (m *Metrics) Handler(logger *slog.Logger) http.Handler {
	return Handler(m.reg, logger)
}

// ServerOptions bounds the listener's header read and graceful shutdown.
type ServerOptions struct {
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
	Logger            *slog.Logger
}

func (o ServerOptions) withDefaults() ServerOptions {
	if o.ReadHeaderTimeout <= 0 {
		o.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	return o
}

// Server is one running /metrics listener. It owns its listener and shuts it
// down when the caller's context is cancelled or Close is called.
type Server struct {
	srv             *http.Server
	ln              net.Listener
	addr            net.Addr
	cancel          context.CancelFunc
	done            chan struct{}
	shutdownTimeout time.Duration
	err             error
}

// Start binds addr synchronously (so a bind error fails startup), then serves
// only GET /metrics. Start never retries or falls back to another port. On
// context cancellation or Close it calls Shutdown with the bounded timeout and
// then Close.
func Start(ctx context.Context, addr string, handler http.Handler, opts ServerOptions) (*Server, error) {
	if handler == nil {
		return nil, errors.New("metrics: handler is required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server, err := serveListener(ctx, ln, handler, opts)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return server, nil
}

// Serve serves handler on an already-bound listener and blocks until the
// listener shuts down. Tests and embedders that need the actual ephemeral
// address use net.Listen("tcp", "127.0.0.1:0") and this function.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, opts ServerOptions) error {
	server, err := serveListener(ctx, ln, handler, opts)
	if err != nil {
		return err
	}
	return server.Wait()
}

func serveListener(ctx context.Context, ln net.Listener, handler http.Handler, opts ServerOptions) (*Server, error) {
	if ln == nil {
		return nil, errors.New("metrics: listener is required")
	}
	if handler == nil {
		return nil, errors.New("metrics: handler is required")
	}
	opts = opts.withDefaults()

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}
	if opts.Logger != nil {
		srv.ErrorLog = slog.NewLogLogger(opts.Logger.Handler(), slog.LevelError)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	server := &Server{
		srv:             srv,
		ln:              ln,
		addr:            ln.Addr(),
		cancel:          cancel,
		done:            make(chan struct{}),
		shutdownTimeout: opts.ShutdownTimeout,
	}
	go server.drain(serveCtx)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.err = err
		close(server.done)
	}()
	return server, nil
}

// Addr returns the actual bound address, which is observable even when Start
// was called with port 0.
func (s *Server) Addr() net.Addr { return s.addr }

// Wait blocks until the listener has stopped and returns its serve error.
func (s *Server) Wait() error {
	<-s.done
	return s.err
}

// Close cancels the listener, drains in-flight requests within the configured
// timeout, and blocks until the server has stopped. It is safe to call more
// than once.
func (s *Server) Close() error {
	s.cancel()
	<-s.done
	return s.err
}

// drain shuts the HTTP server down once the caller's context ends or the
// server stops for another reason.
func (s *Server) drain(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.done:
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		_ = s.srv.Close()
	}
}

// StartForProcess starts the process /metrics listener with the validated
// config address, logs the actual bound address, and warns when the address is
// not loopback. The listener has no authentication, so a non-loopback bind is
// an explicit operator decision and network policy is the control.
func StartForProcess(
	ctx context.Context,
	m *Metrics,
	addr string,
	configKey string,
	logger *slog.Logger,
	opts ServerOptions,
) (*Server, error) {
	if m == nil {
		return nil, errors.New("metrics: metrics instance is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	loopback, err := IsLoopbackAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", configKey, addr, err)
	}
	if !loopback {
		logger.Warn("metrics listener is not loopback; the endpoint has no authentication",
			"config_key", configKey, "addr", addr)
	}
	opts.Logger = logger
	server, err := Start(ctx, addr, m.Handler(logger), opts)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", configKey, addr, err)
	}
	logger.Info("metrics listener started",
		"component", m.Component(),
		"config_key", configKey,
		"addr", server.Addr().String())
	return server, nil
}

// IsLoopbackAddr reports whether addr is a loopback host:port. Empty hosts
// (all interfaces), non-IP hostnames other than localhost, and unspecified
// addresses are not loopback; callers warn for those.
func IsLoopbackAddr(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if host == "" {
		return false, nil
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, nil
	}
	return ip.IsLoopback(), nil
}
