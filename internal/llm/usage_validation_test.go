package llm

import (
	"context"
	"io"
	"net/http"
	"testing"
)

// A malformed 2xx response must never enter the money path with negative values.
func TestOpenCodeRejectsInvalidUsage(t *testing.T) {
	// covers: FIN-01
	tests := []struct {
		name  string
		usage string
	}{
		{"negative prompt tokens", `{"prompt_tokens":-1,"completion_tokens":1,"total_tokens":0}`},
		{"negative completion tokens", `{"prompt_tokens":1,"completion_tokens":-1,"total_tokens":0}`},
		{"negative total tokens", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":-1}`},
		{"negative cached tokens", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":-1}}`},
		{"negative reasoning detail tokens", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":-1}}`},
		{"negative opencode input", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"input":-1}}}`},
		{"negative opencode output", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"output":-1}}}`},
		{"negative opencode reasoning", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"reasoning":-1}}}`},
		{"negative opencode cache read", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"cache_read":-1}}}`},
		{"negative opencode cache write 5m", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"cache_write_5m":-1}}}`},
		{"negative opencode cache write 1h", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"tokens":{"cache_write_1h":-1}}}`},
		{"negative api cost", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"cost":-0.01}`},
		{"negative list cost number", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"list_cost":-5}}`},
		{"negative list cost string", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"list_cost":"-5"}}`},
		{"negative allowance number", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"allowance":-5}}`},
		{"negative allowance string", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"allowance":"-5"}}`},
		{"total below components", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":10}`},
		{"total above components", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":12}`},
		{"total without components", `{"total_tokens":1}`},
		{"token count above maximum", `{"prompt_tokens":1099511627777,"completion_tokens":0,"total_tokens":1099511627777}`},
		{"total count above maximum", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":1099511627777}`},
		{"max int prompt and completion", `{"prompt_tokens":9223372036854775807,"completion_tokens":9223372036854775807,"total_tokens":9223372036854775807}`},
		{"huge list cost", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"list_cost":1e100}}`},
		{"huge allowance", `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"opencode":{"allowance":1e100}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + tt.usage + `}`
			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			adapter, _ := newTestAdapter(t, server, nil)
			if _, err := adapter.Complete(context.Background(), testRequest()); err == nil {
				t.Fatalf("Complete accepted invalid usage: %s", tt.usage)
			} else {
				pe, ok := AsProviderError(err)
				if !ok || pe.Kind != KindInvalidResponse || pe.Retryable {
					t.Fatalf("error = %+v, want invalid_response non-retryable", err)
				}
			}
		})
	}
}

func TestOpenCodeAcceptsValidUsageAtBoundaries(t *testing.T) {
	// covers: FIN-01
	tests := []struct {
		name  string
		usage string
	}{
		{"zero", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`},
		{"positive", `{"prompt_tokens":38,"completion_tokens":16,"total_tokens":54,"cost":0,"opencode":{"list_cost":0.0000153,"allowance":0.00000918}}`},
		{"derived total", `{"prompt_tokens":38,"completion_tokens":16}`},
		{"maximum token count", `{"prompt_tokens":1099511627776,"completion_tokens":0,"total_tokens":1099511627776}`},
		{"zero boundary costs", `{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":0,"opencode":{"list_cost":0,"allowance":0}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"id":"1","model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":` + tt.usage + `}`
			server := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			})
			adapter, _ := newTestAdapter(t, server, nil)
			got, err := adapter.Complete(context.Background(), testRequest())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.Usage.CostUSD.IsNegative() {
				t.Fatalf("CostUSD = %s, want non-negative", got.Usage.CostUSD)
			}
		})
	}
}
