package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// Defaults for the reusable webhook signature middleware.
const (
	DefaultPrefix      = "v0="
	DefaultTolerance   = 5 * time.Minute
	DefaultMaxBodySize = 1 << 20
)

// Error codes written by the fallback envelope. The API passes its own
// WriteError so the codes and request IDs are identical everywhere.
const (
	CodeUnauthenticated  = "unauthenticated"
	CodeReplay           = "replay"
	CodeConflict         = "conflict"
	CodePayloadTooLarge  = "payload_too_large"
	MessageUnauthorized  = "signature verification failed"
	MessageReplay        = "request replay detected"
	MessageConflict      = "request could not be accepted"
	MessagePayloadTooBig = "request body is too large"
)

// Options configures the webhook signature middleware. Secret and both header
// names are required.
type Options struct {
	// Secret is the shared HMAC secret. It is never logged.
	Secret []byte
	// SignatureHeader names the header carrying <Prefix><hex>, for example
	// "X-Slack-Signature".
	SignatureHeader string
	// TimestampHeader names the header carrying Unix seconds, for example
	// "X-Slack-Request-Timestamp".
	TimestampHeader string
	// Prefix is the signature prefix; it defaults to "v0=".
	Prefix string
	// Tolerance is the accepted absolute timestamp skew; it defaults to 5m.
	Tolerance time.Duration
	// MaxBodyBytes caps the raw body; it defaults to 1 MiB.
	MaxBodyBytes int64
	// Replay is the replay marker store; nil creates a new in-memory cache.
	Replay ReplayCache
	// Clock supplies the current time. Nil defaults to clock.Real{} at the
	// call site so tests can inject clock.Fake.
	Clock clock.Clock
	// Logger records structured rejection reasons. Nil uses slog.Default().
	Logger *slog.Logger
	// WriteError writes the shared JSON error envelope. Nil writes a minimal
	// envelope carrying the X-Request-ID response header.
	WriteError func(w http.ResponseWriter, r *http.Request, status int, code, message string)
}

// String renders the non-secret options; Secret is always replaced.
func (o Options) String() string {
	return fmt.Sprintf("webhook.Options{signature_header:%q,timestamp_header:%q,prefix:%q,tolerance:%s,max_body_bytes:%d,secret:<redacted>}",
		o.SignatureHeader, o.TimestampHeader, o.Prefix, o.Tolerance, o.MaxBodyBytes)
}

// GoString keeps %#v from falling back to the raw struct fields.
func (o Options) GoString() string { return o.String() }

// Format covers every verb, including %v, %+v, %#v, and %s.
func (o Options) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, o.String())
}

// Verifier validates webhook signatures, timestamp tolerance, and exact
// replays before handing the restored body to the downstream handler.
type Verifier struct {
	secret          []byte
	signatureHeader string
	timestampHeader string
	prefix          string
	tolerance       time.Duration
	maxBodyBytes    int64
	replay          ReplayCache
	clock           clock.Clock
	logger          *slog.Logger
	writeError      func(http.ResponseWriter, *http.Request, int, string, string)
}

// New validates opts and returns a verifier. The error names a missing field
// and never includes the secret.
func New(opts Options) (*Verifier, error) {
	if len(opts.Secret) == 0 {
		return nil, errors.New("webhook: secret is required")
	}
	if strings.TrimSpace(opts.SignatureHeader) == "" {
		return nil, errors.New("webhook: signature header name is required")
	}
	if strings.TrimSpace(opts.TimestampHeader) == "" {
		return nil, errors.New("webhook: timestamp header name is required")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	if strings.TrimSuffix(prefix, "=") == "" {
		return nil, errors.New("webhook: signature prefix is empty")
	}
	tolerance := opts.Tolerance
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	if tolerance < 0 {
		return nil, errors.New("webhook: tolerance must be positive")
	}
	maxBody := opts.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodySize
	}
	if maxBody < 0 {
		return nil, errors.New("webhook: max body size must be positive")
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	replay := opts.Replay
	if replay == nil {
		replay = NewMemoryReplayCache(0)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	writeError := opts.WriteError
	if writeError == nil {
		writeError = writeFallbackError
	}
	return &Verifier{
		secret:          bytes.Clone(opts.Secret),
		signatureHeader: opts.SignatureHeader,
		timestampHeader: opts.TimestampHeader,
		prefix:          prefix,
		tolerance:       tolerance,
		maxBodyBytes:    maxBody,
		replay:          replay,
		clock:           clk,
		logger:          logger,
		writeError:      writeError,
	}, nil
}

// Middleware wraps next with signature, timestamp, and replay validation.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limited := http.MaxBytesReader(w, r.Body, v.maxBodyBytes)
		defer func() { _ = limited.Close() }()
		body, err := io.ReadAll(limited)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				v.reject(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, MessagePayloadTooBig, "too_large")
				return
			}
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "body")
			return
		}

		timestampValues := r.Header.Values(v.timestampHeader)
		if len(timestampValues) != 1 {
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "timestamp")
			return
		}
		timestampText := timestampValues[0]
		timestamp, ok := parseTimestamp(timestampText)
		if !ok {
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "timestamp")
			return
		}

		signatureValues := r.Header.Values(v.signatureHeader)
		if len(signatureValues) != 1 {
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "signature")
			return
		}
		signatureValue := signatureValues[0]
		signature, ok := parseSignature(signatureValue, v.prefix)
		if !ok {
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "signature")
			return
		}

		now := v.clock.Now()
		delta := now.Sub(time.Unix(timestamp, 0))
		if delta < 0 {
			delta = -delta
		}
		if delta > v.tolerance {
			reason := "stale"
			if time.Unix(timestamp, 0).After(now) {
				reason = "future"
			}
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, reason)
			return
		}

		mac := hmac.New(sha256.New, v.secret)
		mac.Write([]byte(strings.TrimSuffix(v.prefix, "=")))
		mac.Write([]byte(":"))
		mac.Write([]byte(timestampText))
		mac.Write([]byte(":"))
		mac.Write(body)
		if !hmac.Equal(mac.Sum(nil), signature) {
			v.reject(w, r, http.StatusUnauthorized, CodeUnauthenticated, MessageUnauthorized, "signature")
			return
		}

		if err := v.replay.CheckAndStore(replayKey(timestampText, signature), now, 2*v.tolerance); err != nil {
			if errors.Is(err, ErrReplay) {
				v.reject(w, r, http.StatusConflict, CodeReplay, MessageReplay, "replay")
				return
			}
			v.reject(w, r, http.StatusConflict, CodeConflict, MessageConflict, "cache_full")
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func (v *Verifier) reject(w http.ResponseWriter, r *http.Request, status int, code, message, reason string) {
	v.logger.Warn("webhook: rejected",
		slog.String("reason", reason),
		slog.String("request_id", w.Header().Get("X-Request-ID")),
		slog.String("trace_id", w.Header().Get("X-Trace-ID")),
	)
	v.writeError(w, r, status, code, message)
}

func parseTimestamp(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	timestamp, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return timestamp, true
}

func parseSignature(value, prefix string) ([]byte, bool) {
	if !strings.HasPrefix(value, prefix) {
		return nil, false
	}
	hexPart := value[len(prefix):]
	if len(hexPart) != sha256.Size*2 {
		return nil, false
	}
	decoded, err := hex.DecodeString(hexPart)
	if err != nil || len(decoded) != sha256.Size {
		return nil, false
	}
	return decoded, true
}

// replayKey derives the canonical cache key for an accepted request. It hashes
// the decoded MAC bytes rather than the header text, so lower-, upper-, and
// mixed-case hex encodings of the same signature share one marker. The HMAC
// already binds the timestamp and raw body, so the timestamp plus MAC uniquely
// identifies the request.
func replayKey(timestampText string, signature []byte) string {
	mac := sha256.New()
	mac.Write([]byte(timestampText))
	mac.Write([]byte("\n"))
	mac.Write(signature)
	return hex.EncodeToString(mac.Sum(nil))
}

func writeFallbackError(w http.ResponseWriter, _ *http.Request, status int, code, message string) {
	requestID := w.Header().Get("X-Request-ID")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}{
		Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: code, Message: message},
		RequestID: requestID,
	})
}

func (v Verifier) redacted() string {
	return fmt.Sprintf("webhook.Verifier{signature_header:%q,timestamp_header:%q,prefix:%q,tolerance:%s,max_body_bytes:%d,secret:<redacted>}",
		v.signatureHeader, v.timestampHeader, v.prefix, v.tolerance, v.maxBodyBytes)
}

// String keeps accidental formatting of a Verifier from exposing the secret.
func (v *Verifier) String() string {
	if v == nil {
		return "<nil>"
	}
	return v.redacted()
}

// GoString keeps %#v from falling back to the raw struct fields.
func (v Verifier) GoString() string { return v.redacted() }

// Format covers every verb for both Verifier values and pointers.
func (v Verifier) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, v.redacted())
}
