package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseErrorBodyFamilies(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantType    string
		wantMessage string
		wantRetry   time.Duration
		wantRetryOK bool
		wantShould  *bool
	}{
		{
			name:        "live wrapper",
			body:        `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`,
			wantType:    "server_error",
			wantMessage: "Upstream request failed: Model is unavailable.",
		},
		{
			name:        "nested type error",
			body:        `{"type":"error","error":{"type":"MissingSessionID","message":"missing","retryAfter":60,"shouldRetry":false}}`,
			wantType:    "MissingSessionID",
			wantMessage: "missing",
			wantRetry:   60 * time.Second,
			wantRetryOK: true,
			wantShould:  boolPtr(false),
		},
		{
			name:        "top-level typed",
			body:        `{"type":"Router.Unavailable","message":"down","retryAfter":"5","shouldRetry":true,"metadata":{"limit":"x"}}`,
			wantType:    "Router.Unavailable",
			wantMessage: "down",
			wantRetry:   5 * time.Second,
			wantRetryOK: true,
			wantShould:  boolPtr(true),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseErrorBody([]byte(tt.body))
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tt.wantMessage)
			}
			if got.RetryAfter != tt.wantRetry {
				t.Errorf("RetryAfter = %s, want %s", got.RetryAfter, tt.wantRetry)
			}
			if (got.RetryAfter != 0) != tt.wantRetryOK {
				t.Errorf("RetryAfter presence = %v, want %v", got.RetryAfter != 0, tt.wantRetryOK)
			}
			if tt.wantShould == nil {
				if got.ShouldRetry != nil {
					t.Errorf("ShouldRetry = %v, want nil", *got.ShouldRetry)
				}
			} else if got.ShouldRetry == nil || *got.ShouldRetry != *tt.wantShould {
				t.Errorf("ShouldRetry = %v, want %v", got.ShouldRetry, *tt.wantShould)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestClassifyProviderErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  ErrorKind
		retryable bool
	}{
		{"401 generic", 401, `{"error":{"type":"server_error","message":"Go requires a credential"}}`, KindAuth, false},
		{"403 forbidden", 403, `{"message":"forbidden"}`, KindAuth, false},
		{"400 unknown model mislabeled", 400, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`, KindModelNotFound, false},
		{"404 model not found", 404, `{"type":"Router.ModelNotFound","message":"model not found"}`, KindModelNotFound, false},
		{"400 missing session", 400, `{"type":"error","error":{"type":"MissingSessionID","message":"missing"}}`, KindInvalidRequest, false},
		{"400 malformed", 400, `{"error":{"type":"invalid_request_error","message":"bad"}}`, KindInvalidRequest, false},
		{"400 context", 400, `{"type":"Generation.EstimatedContextLimitExceeded","message":"too long"}`, KindContextLength, false},
		{"400 body too large", 400, `{"type":"Generation.UpstreamBodyTooLarge","message":"large"}`, KindContextLength, false},
		{"400 deprecated", 400, `{"type":"Generation.ModelDeprecated","message":"old"}`, KindInvalidRequest, false},
		{"400 unsupported", 400, `{"type":"ResponsesRequest.Unsupported","message":"no"}`, KindInvalidRequest, false},
		{"400 budget", 400, `{"type":"Account.GoUsageLimit","message":"limit","retryAfter":60}`, KindBudget, false},
		{"400 insufficient funds", 400, `{"type":"Account.InsufficientFunds","message":"funds"}`, KindBudget, false},
		{"400 region", 400, `{"type":"Account.RegionUnavailable","message":"region"}`, KindAccountRestricted, false},
		{"400 training", 400, `{"type":"Account.TrainingNotAllowed","message":"training"}`, KindAccountRestricted, false},
		{"429", 429, `{"type":"RateLimit.Exceeded","message":"slow","retryAfter":3}`, KindRateLimit, true},
		{"529", 529, `{"message":"overloaded"}`, KindOverloaded, true},
		{"408", 408, `{"type":"Timeout","message":"timeout"}`, KindTimeout, true},
		{"500 proxy timeout", 500, `{"type":"Proxy.ResponseTimeout","message":"timeout"}`, KindTimeout, true},
		{"500 proxy connection", 500, `{"type":"Proxy.ConnectionFailed","message":"reset"}`, KindUnavailable, true},
		{"500 router unavailable", 500, `{"type":"Router.Unavailable","message":"down"}`, KindUnavailable, true},
		{"500 route unavailable", 500, `{"type":"Route.Unavailable","message":"down"}`, KindUnavailable, true},
		{"500 cache unavailable", 500, `{"type":"Cache.Unavailable","message":"down"}`, KindUnavailable, true},
		{"500 account unavailable", 500, `{"type":"Account.Unavailable","message":"down"}`, KindUnavailable, true},
		{"500 generic", 500, `{"error":{"type":"server_error","message":"boom"}}`, KindServer, true},
		{"502 generic", 502, `oops`, KindServer, true},
		{"418 unknown 4xx", 418, `{"message":"teapot"}`, KindInvalidRequest, false},
		{"409 unknown 4xx", 409, `{}`, KindInvalidRequest, false},
		{"500 unknown body", 500, `{}`, KindServer, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyProviderError(ProviderOpenCodeGo, "deepseek-v4.1-flash", tt.status, []byte(tt.body))
			if err.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", err.Kind, tt.wantKind)
			}
			if err.Retryable != tt.retryable {
				t.Errorf("Retryable = %v, want %v", err.Retryable, tt.retryable)
			}
			if err.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", err.StatusCode, tt.status)
			}
		})
	}
}

func TestShouldRetryRules(t *testing.T) {
	downgrade := classifyProviderError(ProviderOpenCodeGo, "m", 500, []byte(`{"type":"Router.Unavailable","message":"down","shouldRetry":false}`))
	if downgrade.Retryable {
		t.Fatal("shouldRetry:false did not downgrade an unavailable error")
	}
	upgrade := classifyProviderError(ProviderOpenCodeGo, "m", 400, []byte(`{"type":"Mystery.Thing","message":"unknown","shouldRetry":true}`))
	if upgrade.Retryable {
		t.Fatal("shouldRetry:true rescued an invalid-request error")
	}
	server := classifyProviderError(ProviderOpenCodeGo, "m", 500, []byte(`{"type":"Proxy.ConnectionFailed","message":"reset","shouldRetry":true}`))
	if !server.Retryable {
		t.Fatal("shouldRetry:true downgraded an availability error")
	}
	fatal := classifyProviderError(ProviderOpenCodeGo, "m", 401, []byte(`{"type":"authentication_error","message":"bad key","shouldRetry":true}`))
	if fatal.Retryable {
		t.Fatal("shouldRetry:true rescued an auth error")
	}
	budget := classifyProviderError(ProviderOpenCodeGo, "m", 400, []byte(`{"type":"Account.GoUsageLimit","message":"limit","shouldRetry":true}`))
	if budget.Retryable {
		t.Fatal("shouldRetry:true rescued a budget error")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"seconds", "5", 5 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative", "-1", 0, false},
		{"malformed", "later", 0, false},
		{"http date", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{"past date", now.Add(-30 * time.Second).Format(http.TimeFormat), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfterHeader(tt.value, now)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("parseRetryAfterHeader(%q) = %s, %v; want %s, %v", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestProviderErrorRedactionAndBodyCap(t *testing.T) {
	key := "oc_sk_synthetic_do_not_log"
	body := `{"error":{"type":"server_error","message":"Bearer ` + key + ` for x-opencode-session: ses_abc123"},"padding":"` + strings.Repeat("x", maxErrorBody) + `"}`
	err := classifyProviderError(ProviderOpenCodeGo, "m", 500, []byte(body))
	if strings.Contains(err.Error(), key) || strings.Contains(string(err.Body), key) {
		t.Fatal("ProviderError leaked a synthetic API key")
	}
	if strings.Contains(err.Error(), "ses_abc123") || strings.Contains(string(err.Body), "ses_abc123") {
		t.Fatal("ProviderError leaked a session ID")
	}
	if len(err.Body) > maxErrorBody {
		t.Fatalf("Body length = %d, want <= %d", len(err.Body), maxErrorBody)
	}
	if strings.Contains(err.Error(), "padding") {
		t.Fatal("ProviderError.Error included the raw body")
	}
}

func TestProviderErrorHelpers(t *testing.T) {
	cause := context.Canceled
	err := &ProviderError{Kind: KindCanceled, Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is did not unwrap the cause")
	}
	pe, ok := AsProviderError(err)
	if !ok || pe.Kind != KindCanceled {
		t.Fatalf("AsProviderError = %v, %v", pe, ok)
	}
	if IsRetryable(err) {
		t.Fatal("IsRetryable(canceled) = true")
	}
	budget := classifyProviderError(ProviderOpenCodeGo, "m", 400, []byte(`{"type":"Account.GoUsageLimit","message":"limit"}`))
	if !IsBudgetExhausted(budget) {
		t.Fatal("IsBudgetExhausted(budget) = false")
	}
	if IsBudgetExhausted(errors.New("other")) {
		t.Fatal("IsBudgetExhausted(other) = true")
	}
	var nilErr *ProviderError
	if nilErr.Error() == "" {
		t.Fatal("nil ProviderError.Error() is empty")
	}
}

func TestProviderErrorJSONBodyRemainsRedacted(t *testing.T) {
	err := classifyProviderError(ProviderOpenCodeGo, "m", 401, []byte(`{"error":{"message":"oc_sk_abc123"}}`))
	if strings.Contains(string(err.Body), "oc_sk_abc123") {
		t.Fatal("raw Body retained the API key")
	}
	var decoded map[string]any
	if json.Unmarshal(err.Body, &decoded) != nil {
		t.Fatalf("redacted body is not JSON: %s", err.Body)
	}
}

func TestProviderErrorRedactsSecretShapedMetadata(t *testing.T) {
	tests := []struct {
		name      string
		err       *ProviderError
		forbidden []string
	}{
		{
			name:      "api key model",
			err:       newUnknownModelError("oc_sk_live_deadbeef"),
			forbidden: []string{"oc_sk_live_deadbeef"},
		},
		{
			name:      "session model",
			err:       &ProviderError{Model: "ses_live_123", Kind: KindInvalidRequest},
			forbidden: []string{"ses_live_123"},
		},
		{
			name:      "bearer model",
			err:       &ProviderError{Model: "Bearer abc.def-123", Kind: KindInvalidRequest},
			forbidden: []string{"abc.def-123"},
		},
		{
			name:      "api key provider and request id",
			err:       &ProviderError{Provider: "oc_sk_live_provider", Model: "m", RequestID: "oc_sk_live_request", Kind: KindServer},
			forbidden: []string{"oc_sk_live_provider", "oc_sk_live_request"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := tt.err.Error()
			for _, secret := range tt.forbidden {
				if strings.Contains(text, secret) {
					t.Errorf("ProviderError.Error() = %q still contains %q", text, secret)
				}
			}
			if tt.err.Message != "" {
				for _, secret := range tt.forbidden {
					if strings.Contains(tt.err.Message, secret) {
						t.Errorf("ProviderError.Message = %q still contains %q", tt.err.Message, secret)
					}
				}
			}
		})
	}
}
