package llm

import (
	"net/http"
	"testing"
)

// TestClassificationCoverageGaps covers the spec §5.9.2 rows that the adapter
// error-body tests do not exercise directly.
func TestClassificationCoverageGaps(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  ErrorKind
		retryable bool
	}{
		{"connection config error", http.StatusBadRequest, `{"type":"Connection.Refused","message":"bad config"}`, KindInvalidRequest, false},
		{"rate limit family without 429", http.StatusOK, `{"type":"RateLimit.FreeUsageLimitError","message":"limit"}`, KindRateLimit, true},
		{"upstream context limit", http.StatusPermanentRedirect, `{"type":"Generation.UpstreamContextLimitExceeded","message":"too long"}`, KindContextLength, false},
		{"account blocked", http.StatusBadRequest, `{"type":"Account.Blocked","message":"blocked"}`, KindAccountRestricted, false},
		{"account disabled", http.StatusBadRequest, `{"type":"Account.Disabled","message":"disabled"}`, KindAccountRestricted, false},
		{"credit limit exceeded", http.StatusBadRequest, `{"type":"Account.CreditLimitExceeded","message":"credit"}`, KindBudget, false},
		{"account limit exceeded", http.StatusBadRequest, `{"type":"Account.LimitExceeded","message":"limit"}`, KindBudget, false},
		{"gateway timeout", http.StatusGatewayTimeout, `{}`, KindServer, true},
		{"request timeout status", http.StatusRequestTimeout, `{}`, KindTimeout, true},
		{"unknown redirect", http.StatusFound, `{}`, KindUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyProviderError(ProviderOpenCodeGo, "m", tt.status, []byte(tt.body))
			if err.Kind != tt.wantKind || err.Retryable != tt.retryable {
				t.Fatalf("kind/retryable = %q/%v, want %q/%v", err.Kind, err.Retryable, tt.wantKind, tt.retryable)
			}
		})
	}
}

func TestFatalKindsNeverRetry(t *testing.T) {
	// covers: spec §6.5 (no FIN ID)
	for _, kind := range []ErrorKind{
		KindAuth, KindInvalidRequest, KindModelNotFound, KindContextLength,
		KindBudget, KindAccountRestricted, KindCircuitOpen, KindCassetteMiss,
		KindCanceled, KindInvalidResponse,
	} {
		if !isFatalKind(kind) {
			t.Errorf("isFatalKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []ErrorKind{KindRateLimit, KindTimeout, KindUnavailable, KindOverloaded, KindServer, KindUnknown} {
		if isFatalKind(kind) {
			t.Errorf("isFatalKind(%q) = true, want false", kind)
		}
	}
}
