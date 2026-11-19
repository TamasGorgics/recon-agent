package slack

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestClientOptionsJSONAndSlogRedactBotToken covers the renderers that bypass
// String/GoString/Format: encoding/json and slog's JSON handler. Both would
// otherwise serialize the exported BotToken field.
func TestClientOptionsJSONAndSlogRedactBotToken(t *testing.T) {
	options := ClientOptions{
		BaseURL:      "https://slack.example.test/api",
		BotToken:     formatTestBotToken,
		Timeout:      3 * time.Second,
		MaxAttempts:  2,
		MaxRetryWait: 5 * time.Second,
	}

	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatalf("json.Marshal(ClientOptions): %v", err)
	}
	assertNoClientOptionsSecret(t, "json.Marshal(ClientOptions)", string(encoded))
	var direct map[string]any
	if err := json.Unmarshal(encoded, &direct); err != nil {
		t.Fatalf("decode ClientOptions JSON: %v", err)
	}
	if got := direct["bot_token"]; got != redactedSecret {
		t.Fatalf("json bot_token = %v, want %q", got, redactedSecret)
	}
	if got := direct["base_url"]; got != options.BaseURL {
		t.Fatalf("json base_url = %v, want %q", got, options.BaseURL)
	}
	if got := direct["max_attempts"]; got != float64(options.MaxAttempts) {
		t.Fatalf("json max_attempts = %v, want %d", got, options.MaxAttempts)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("client options",
		slog.Any("options", options),
		slog.Any("options_pointer", &options),
	)
	logged := buf.String()
	assertNoClientOptionsSecret(t, "slog JSON handler", logged)

	var line map[string]any
	if err := json.Unmarshal([]byte(logged), &line); err != nil {
		t.Fatalf("decode slog JSON line: %v", err)
	}
	for _, key := range []string{"options", "options_pointer"} {
		group, ok := line[key].(map[string]any)
		if !ok {
			t.Fatalf("slog %s = %T, want an object: %s", key, line[key], logged)
		}
		if got := group["bot_token"]; got != redactedSecret {
			t.Fatalf("slog %s.bot_token = %v, want %q", key, got, redactedSecret)
		}
		if got := group["base_url"]; got != options.BaseURL {
			t.Fatalf("slog %s.base_url = %v, want %q", key, got, options.BaseURL)
		}
		if got := group["max_attempts"]; got != float64(options.MaxAttempts) {
			t.Fatalf("slog %s.max_attempts = %v, want %d", key, got, options.MaxAttempts)
		}
	}
}

func assertNoClientOptionsSecret(t *testing.T, label, out string) {
	t.Helper()
	for _, forbidden := range []string{formatTestBotToken, "Bearer"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%s leaked %q: %s", label, forbidden, out)
		}
	}
}
