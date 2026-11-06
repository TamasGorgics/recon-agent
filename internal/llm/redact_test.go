package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactPatterns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"api key", "key=oc_sk_abc123_DEF", []string{"oc_sk_abc123_DEF"}},
		{"bearer", "Authorization: Bearer abc.def-123", []string{"abc.def-123"}},
		{"authorization", "authorization=topsecret", []string{"topsecret"}},
		{"session header", "x-opencode-session: ses_abc-123", []string{"ses_abc-123"}},
		{"session bare", "reused ses_abc-123 across calls", []string{"ses_abc-123"}},
		{"multiline", "x-opencode-session: ses_abc\nAuthorization: Bearer oc_sk_key\n", []string{"ses_abc", "oc_sk_key"}},
		{"authorization before newline", "authorization:sekrit-token\nnext", []string{"sekrit-token"}},
		{"session before newline", "x-opencode-session: ses_secret123\nnext", []string{"ses_secret123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			for _, secret := range tt.want {
				if strings.Contains(got, secret) {
					t.Errorf("Redact(%q) = %q still contains %q", tt.in, got, secret)
				}
			}
			if again := Redact(got); again != got {
				t.Errorf("Redact is not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestRedactLeavesOrdinaryText(t *testing.T) {
	input := "model deepseek-v4.1-flash cost 0.0000153 tokens 38/16 https://opencode.ai/inference/go/openai/v1"
	if got := Redact(input); got != input {
		t.Fatalf("Redact changed ordinary text: %q", got)
	}
}

func TestRedactBytes(t *testing.T) {
	input := []byte(`{"error":"Bearer oc_sk_synthetic123","session":"ses_abc"}`)
	got := string(RedactBytes(input))
	if strings.Contains(got, "oc_sk_synthetic123") || strings.Contains(got, "ses_abc") {
		t.Fatalf("RedactBytes left a secret: %s", got)
	}
	if !strings.Contains(got, "Redacted") && !strings.Contains(got, "REDACTED") {
		t.Fatalf("RedactBytes did not mark redaction: %s", got)
	}
}

func TestRedactPreservesJSONStructure(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		secrets []string
	}{
		{"authorization at string end", `{"error":{"message":"authorization:sekrit-token"}}`, []string{"sekrit-token"}},
		{"authorization before comma", `{"error":{"message":"authorization:sekrit-token, rest"}}`, []string{"sekrit-token"}},
		{"authorization before brace", `{"error":{"message":"authorization:sekrit-token}"}}`, []string{"sekrit-token"}},
		{"authorization json key", `{"authorization":"sekrit-token"}`, []string{"sekrit-token"}},
		{"session header at string end", `{"error":{"message":"x-opencode-session: ses_secret123"}}`, []string{"ses_secret123"}},
		{"session header json key", `{"x-opencode-session":"ses_secret123"}`, []string{"ses_secret123"}},
		{"bearer at string end", `{"error":{"message":"Bearer abc.def-123"}}`, []string{"abc.def-123"}},
		{"bearer before comma", `{"error":{"message":"Bearer abc.def-123, next"}}`, []string{"abc.def-123"}},
		{"pretty json", "{\n  \"message\": \"authorization:sekrit-token\"\n}\n", []string{"sekrit-token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if !json.Valid([]byte(got)) {
				t.Fatalf("Redact produced invalid JSON: %q", got)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("Redact(%q) = %q still contains %q", tt.in, got, secret)
				}
			}
			if again := Redact(got); again != got {
				t.Errorf("Redact is not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestProviderErrorBodyPreservesJSONStructure(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		secrets []string
	}{
		{"authorization message", `{"error":{"message":"authorization:sekrit-token"}}`, []string{"sekrit-token"}},
		{"authorization before comma", `{"error":{"message":"authorization:sekrit-token, rest"}}`, []string{"sekrit-token"}},
		{"authorization header key", `{"authorization":"sekrit-token"}`, []string{"sekrit-token"}},
		{"session message", `{"error":{"message":"x-opencode-session: ses_secret123"}}`, []string{"ses_secret123"}},
		{"bearer message", `{"error":{"message":"Bearer abc.def-123"}}`, []string{"abc.def-123"}},
		{"pretty body", "{\n  \"error\": {\n    \"message\": \"authorization:sekrit-token\"\n  }\n}", []string{"sekrit-token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyProviderError(ProviderOpenCodeGo, "m", 401, []byte(tt.body))
			if !json.Valid(err.Body) {
				t.Fatalf("ProviderError.Body is not JSON: %q", err.Body)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(string(err.Body), secret) {
					t.Errorf("ProviderError.Body = %q still contains %q", err.Body, secret)
				}
			}
		})
	}
}
