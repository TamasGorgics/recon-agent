package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

type requestIDKey struct{}

// requestState is the request-scoped, mutable companion to the immutable
// context values. Authentication runs inside the access-log middleware, so it
// publishes the principal here for the log line after the handler returns.
type requestState struct {
	principal Principal
}

type requestStateKey struct{}

func requestStateFrom(ctx context.Context) *requestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	return state
}

var fallbackIDCounter atomic.Uint64

// RequestIDFromContext returns the request correlation ID. It is never used
// for authorization or audit identity.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func traceIDFromContext(ctx context.Context) string {
	return telemetry.TraceID(ctx)
}

// withRequestContext assigns the request ID and trace ID, stores the trace ID
// in the context, and exposes both on the response before any handler runs.
func (a *API) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := incomingRequestID(r.Header.Values("X-Request-ID"))
		if requestID == "" {
			requestID = a.randomID(16)
		}
		traceID := ""
		if parsed, ok := parseTraceparent(r.Header.Values("Traceparent")); ok {
			traceID = parsed
		} else {
			traceID = a.randomID(16)
		}

		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Trace-ID", traceID)

		ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
		ctx = context.WithValue(ctx, requestStateKey{}, &requestState{})
		ctx = telemetry.WithTraceID(ctx, traceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func incomingRequestID(values []string) string {
	if len(values) != 1 {
		return ""
	}
	value := values[0]
	if len(value) < 1 || len(value) > 128 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return ""
		}
	}
	return value
}

// parseTraceparent accepts exactly one W3C version-00 traceparent with a
// non-zero trace and parent ID. It returns the lower-case 32-hex trace ID.
func parseTraceparent(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Split(values[0], "-")
	if len(parts) != 4 || parts[0] != "00" {
		return "", false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", false
	}
	traceBytes, err := hex.DecodeString(parts[1])
	if err != nil || len(traceBytes) != 16 || allZero(traceBytes) {
		return "", false
	}
	parentBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(parentBytes) != 8 || allZero(parentBytes) {
		return "", false
	}
	if _, err := hex.DecodeString(parts[3]); err != nil {
		return "", false
	}
	return strings.ToLower(parts[1]), true
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func (a *API) randomID(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	// crypto/rand failure is effectively unrecoverable, but correlation must
	// still work: fall back to a digest of the injected clock and a counter.
	seed := strconv.FormatInt(a.clock.Now().UnixNano(), 10) + ":" +
		strconv.FormatUint(fallbackIDCounter.Add(1), 10)
	sum := sha256.Sum256([]byte(seed))
	copy(buffer, sum[:size])
	return hex.EncodeToString(buffer)
}

// recovery catches handler panics, logs only correlation fields, and writes
// the standard 500 envelope when no response has been committed.
func (a *API) recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder, ok := w.(*recordingWriter)
		if !ok {
			recorder = &recordingWriter{ResponseWriter: w}
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("panic recovered",
					slog.String("request_id", RequestIDFromContext(r.Context())),
					slog.String("trace_id", traceIDFromContext(r.Context())),
					slog.String("route", routePattern(r)),
				)
				if !recorder.Written() {
					a.writeError(recorder, r, http.StatusInternalServerError, codeInternal, messageInternal)
				}
			}
		}()
		next.ServeHTTP(recorder, r)
	})
}

// accessLog emits one JSON line per request with correlation, route pattern,
// status, duration, and principal. It never logs headers, query strings, or
// bodies.
func (a *API) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder, ok := w.(*recordingWriter)
		if !ok {
			recorder = &recordingWriter{ResponseWriter: w}
		}
		start := a.clock.Now()
		defer func() {
			principal := requestStateFrom(r.Context()).principal
			role := ""
			if principal.Role.Valid() {
				role = string(principal.Role)
			}
			a.logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("route", routePattern(r)),
				slog.Int("status", recorder.Status()),
				slog.Int64("duration_ms", a.clock.Since(start).Milliseconds()),
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("trace_id", traceIDFromContext(r.Context())),
				slog.String("key_id", principal.KeyID),
				slog.String("role", role),
			)
		}()
		next.ServeHTTP(recorder, r)
	})
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}

// recordingWriter records the committed status without changing the response.
type recordingWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// Written reports whether a status line was committed.
func (w *recordingWriter) Written() bool { return w.wrote }

// Status returns the committed status, or 200 when the handler wrote nothing.
func (w *recordingWriter) Status() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.status
}

// Flush forwards to the underlying writer when it supports flushing.
func (w *recordingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
