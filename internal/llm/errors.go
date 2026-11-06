package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// ErrorKind classifies a provider failure for retry and worker decisions.
type ErrorKind string

const (
	KindAuth              ErrorKind = "auth"
	KindRateLimit         ErrorKind = "rate_limit"
	KindTimeout           ErrorKind = "timeout"
	KindUnavailable       ErrorKind = "unavailable"
	KindOverloaded        ErrorKind = "overloaded"
	KindServer            ErrorKind = "server"
	KindInvalidRequest    ErrorKind = "invalid_request"
	KindModelNotFound     ErrorKind = "model_not_found"
	KindContextLength     ErrorKind = "context_length"
	KindBudget            ErrorKind = "budget"
	KindAccountRestricted ErrorKind = "account_restricted"
	KindCircuitOpen       ErrorKind = "circuit_open"
	KindCassetteMiss      ErrorKind = "cassette_miss"
	KindCanceled          ErrorKind = "canceled"
	KindInvalidResponse   ErrorKind = "invalid_response"
	KindUnknown           ErrorKind = "unknown"
)

// Sentinels returned by the package.
var (
	ErrCassetteMiss      = errors.New("llm: cassette miss")
	ErrScriptedExhausted = errors.New("llm: scripted responses exhausted")
	ErrUnknownModel      = errors.New("llm: unknown model")
	ErrNotConfigured     = errors.New("llm: not configured")
)

// ProviderError is the only provider-failure type that crosses the package
// boundary. Body is redacted and capped; Error never prints it.
type ProviderError struct {
	Provider   string
	Model      string
	StatusCode int
	Kind       ErrorKind
	Retryable  bool
	RetryAfter time.Duration
	RequestID  string
	Message    string
	Body       json.RawMessage
	Attempts   int
	Cause      error
}

// Error returns a metadata-only, redacted description.
func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString("llm provider error")
	if e.Provider != "" {
		b.WriteString(" provider=")
		b.WriteString(Redact(e.Provider))
	}
	if e.Model != "" {
		b.WriteString(" model=")
		b.WriteString(Redact(e.Model))
	}
	if e.StatusCode != 0 {
		b.WriteString(" status=")
		b.WriteString(strconv.Itoa(e.StatusCode))
	}
	b.WriteString(" kind=")
	b.WriteString(string(e.Kind))
	b.WriteString(" retryable=")
	b.WriteString(strconv.FormatBool(e.Retryable))
	if e.RequestID != "" {
		b.WriteString(" request_id=")
		b.WriteString(Redact(e.RequestID))
	}
	if e.Message != "" {
		b.WriteString(" message=")
		b.WriteString(Redact(e.Message))
	}
	return b.String()
}

// Unwrap returns the underlying cause.
func (e *ProviderError) Unwrap() error { return e.Cause }

// AsProviderError returns the typed provider error in err's chain.
func AsProviderError(err error) (*ProviderError, bool) {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// IsRetryable reports whether err is a retryable provider error.
func IsRetryable(err error) bool {
	pe, ok := AsProviderError(err)
	return ok && pe.Retryable
}

// IsBudgetExhausted reports whether err represents budget exhaustion.
func IsBudgetExhausted(err error) bool {
	pe, ok := AsProviderError(err)
	return ok && pe.Kind == KindBudget
}

const maxErrorBody = 8 << 10

type parsedErrorBody struct {
	Type        string
	Message     string
	RetryAfter  time.Duration
	ShouldRetry *bool
	RequestID   string
}

// parseErrorBody extracts the fields shared by the three observed error
// families:
//
//	{"error":{"type":"server_error","message":"..."}}
//	{"type":"error","error":{"type":"MissingSessionID",...}}
//	{"type":"Router.Unavailable","message":"...","retryAfter":5}
func parseErrorBody(body []byte) parsedErrorBody {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil || root == nil {
		return parsedErrorBody{}
	}

	var out parsedErrorBody
	topType := anyString(root["type"])
	nested, _ := root["error"].(map[string]any)
	nestedType := anyString(nested["type"])

	if topType != "" && topType != "error" {
		out.Type = topType
	} else if nestedType != "" {
		out.Type = nestedType
	} else {
		out.Type = topType
	}
	if m := anyString(nested["message"]); m != "" {
		out.Message = m
	} else {
		out.Message = anyString(root["message"])
	}
	out.RequestID = firstNonEmpty(anyString(root["request_id"]), anyString(nested["request_id"]))
	if d, ok := retryAfterFromMaps(root, nested); ok {
		out.RetryAfter = d
	} else if metadata, ok := nested["metadata"].(map[string]any); ok {
		if d, ok := retryAfterFromMaps(metadata); ok {
			out.RetryAfter = d
		}
	} else if metadata, ok := root["metadata"].(map[string]any); ok {
		if d, ok := retryAfterFromMaps(metadata); ok {
			out.RetryAfter = d
		}
	}
	if v, ok := anyBool(root["shouldRetry"]); ok {
		out.ShouldRetry = &v
	} else if v, ok := anyBool(nested["shouldRetry"]); ok {
		out.ShouldRetry = &v
	}
	return out
}

func retryAfterFromMaps(maps ...map[string]any) (time.Duration, bool) {
	for _, m := range maps {
		if m == nil {
			continue
		}
		if d, ok := anySeconds(m["retryAfter"]); ok {
			return d, true
		}
	}
	return 0, false
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func anyBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		b, err := strconv.ParseBool(x)
		return b, err == nil
	default:
		return false, false
	}
}

func anySeconds(v any) (time.Duration, bool) {
	switch x := v.(type) {
	case json.Number:
		return secondsDuration(x.String())
	case string:
		return secondsDuration(x)
	default:
		return 0, false
	}
}

func secondsDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	secs, err := decimal.NewFromString(raw)
	if err != nil || secs.IsNegative() {
		return 0, false
	}
	const maxSeconds = int64(365 * 24 * 60 * 60)
	ms := secs.Mul(decimal.NewFromInt(int64(time.Second))).IntPart()
	maxMS := maxSeconds * int64(time.Millisecond)
	if ms > maxMS {
		ms = maxMS
	}
	return time.Duration(ms), true
}

// parseRetryAfterHeader parses Retry-After seconds or an HTTP date. The header
// wins over body values when both are present.
func parseRetryAfterHeader(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if d, ok := secondsDuration(value); ok {
		return d, true
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// classifyProviderError builds a typed error from an HTTP status and body.
func classifyProviderError(provider, model string, status int, body []byte) *ProviderError {
	parsed := parseErrorBody(body)
	kind, retryable := classify(status, parsed.Type, parsed.Message)
	if parsed.ShouldRetry != nil {
		if !*parsed.ShouldRetry {
			retryable = false
		} else if !isFatalKind(kind) {
			retryable = true
		}
	}
	message := parsed.Message
	if message == "" {
		message = http.StatusText(status)
	}
	if message == "" {
		message = "provider error"
	}
	return &ProviderError{
		Provider:   provider,
		Model:      model,
		StatusCode: status,
		Kind:       kind,
		Retryable:  retryable,
		RetryAfter: parsed.RetryAfter,
		RequestID:  parsed.RequestID,
		Message:    Redact(message),
		Body:       redactedBody(body),
		Attempts:   1,
	}
}

func redactedBody(body []byte) json.RawMessage {
	redacted := RedactBytes(body)
	if len(redacted) > maxErrorBody {
		redacted = redacted[:maxErrorBody]
	}
	return append(json.RawMessage(nil), redacted...)
}

func classify(status int, typ, message string) (ErrorKind, bool) {
	t := strings.ToLower(strings.TrimSpace(typ))
	m := strings.ToLower(message)
	has := func(substrs ...string) bool {
		for _, s := range substrs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}

	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		t == "authentication_error" || strings.Contains(t, "invalidcredential") ||
		has("requires a credential", "invalid credential", "invalid api key"):
		return KindAuth, false
	case t == "account.gousagelimit" || t == "account.insufficientfunds" ||
		t == "account.creditlimitexceeded" || t == "account.limitexceeded":
		return KindBudget, false
	case t == "account.blocked" || t == "account.disabled" ||
		t == "account.regionunavailable" || t == "account.trainingnotallowed":
		return KindAccountRestricted, false
	case (status == http.StatusBadRequest || status == http.StatusNotFound) &&
		(t == "router.modelnotfound" || has("model is unavailable", "model not found", "does not exist")):
		return KindModelNotFound, false
	case t == "generation.estimatedcontextlimitexceeded" ||
		t == "generation.upstreamcontextlimitexceeded" ||
		t == "generation.upstreambodytoolarge" || has("context length"):
		return KindContextLength, false
	case t == "missingsessionid" || t == "generation.modeldeprecated" ||
		t == "responsesrequest.unsupported" || strings.HasPrefix(t, "connection.") ||
		(status == http.StatusBadRequest && t == ""):
		return KindInvalidRequest, false
	case status == http.StatusTooManyRequests || t == "ratelimit" || strings.HasPrefix(t, "ratelimit."):
		return KindRateLimit, true
	case status == 529 || has("overloaded"):
		return KindOverloaded, true
	case status == http.StatusRequestTimeout || t == "timeout" || t == "proxy.responsetimeout":
		return KindTimeout, true
	case t == "proxy.connectionfailed" || t == "router.unavailable" ||
		t == "route.unavailable" || t == "cache.unavailable" || t == "account.unavailable":
		return KindUnavailable, true
	case status >= 500:
		return KindServer, true
	case status >= 400:
		return KindInvalidRequest, false
	case status == http.StatusOK:
		return KindInvalidResponse, false
	default:
		return KindUnknown, false
	}
}

func isFatalKind(kind ErrorKind) bool {
	switch kind {
	case KindAuth, KindInvalidRequest, KindModelNotFound, KindContextLength,
		KindBudget, KindAccountRestricted, KindCircuitOpen, KindCassetteMiss,
		KindCanceled, KindInvalidResponse:
		return true
	default:
		return false
	}
}

// newInvalidResponseError builds the typed error for a 2xx body that cannot be
// mapped.
func newInvalidResponseError(provider, model, message string, body []byte) *ProviderError {
	return &ProviderError{
		Provider:  provider,
		Model:     model,
		Kind:      KindInvalidResponse,
		Retryable: false,
		Message:   Redact(message),
		Body:      redactedBody(body),
		Attempts:  1,
	}
}

// newRequestError builds a pre-flight invalid-request error.
func newRequestError(provider, model, message string) *ProviderError {
	return &ProviderError{
		Provider:  provider,
		Model:     model,
		Kind:      KindInvalidRequest,
		Retryable: false,
		Message:   Redact(message),
	}
}

// newUnknownModelError wraps ErrUnknownModel for a registry miss before HTTP.
// The model is redacted at construction so neither the struct fields nor the
// wrapped cause can leak a secret-shaped value.
func newUnknownModelError(model string) *ProviderError {
	id := Redact(APIID(model))
	return &ProviderError{
		Provider:  ProviderOpenCodeGo,
		Model:     id,
		Kind:      KindModelNotFound,
		Retryable: false,
		Message:   fmt.Sprintf("unknown model %q", id),
		Cause:     fmt.Errorf("%w: %s", ErrUnknownModel, id),
	}
}
