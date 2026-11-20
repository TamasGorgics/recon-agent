package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	slackJSONTestBotToken      = "xoxb-json-secret-bot-token-do-not-leak"
	slackJSONTestSigningSecret = "json-signing-secret-do-not-leak"
)

func writeSlackConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slack.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write slack config: %v", err)
	}
	return path
}

func TestSlackConfigValidate(t *testing.T) {
	if err := (SlackConfig{}).Validate(); err != nil {
		t.Fatalf("empty SlackConfig.Validate() = %v, want nil", err)
	}
	valid := SlackConfig{
		ChannelID:       "C0123456789",
		CaseFileBaseURL: "https://cases.example.test/v1/cases",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid SlackConfig.Validate() = %v", err)
	}

	tests := []struct {
		name string
		cfg  SlackConfig
		want string
	}{
		{"whitespace channel", SlackConfig{ChannelID: "  "}, "slack.channel_id"},
		{"channel surrounding whitespace", SlackConfig{ChannelID: " C0123456789 "}, "surrounding whitespace"},
		{"relative case url", SlackConfig{CaseFileBaseURL: "/v1/cases"}, "slack.case_file_base_url"},
		{"unsupported scheme", SlackConfig{CaseFileBaseURL: "ftp://cases.example.test"}, "slack.case_file_base_url"},
		{"missing host", SlackConfig{CaseFileBaseURL: "https://"}, "slack.case_file_base_url"},
		{"opaque url", SlackConfig{CaseFileBaseURL: "http:cases.example.test"}, "slack.case_file_base_url"},
		{"userinfo", SlackConfig{CaseFileBaseURL: "https://user:pass@cases.example.test"}, "slack.case_file_base_url"},
		{"query", SlackConfig{CaseFileBaseURL: "https://cases.example.test?x=1"}, "slack.case_file_base_url"},
		{"fragment", SlackConfig{CaseFileBaseURL: "https://cases.example.test#fragment"}, "slack.case_file_base_url"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid Slack block")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestSlackConfigNormalized(t *testing.T) {
	cfg := SlackConfig{
		ChannelID:       " C0123456789 ",
		CaseFileBaseURL: "https://cases.example.test/v1/cases/",
	}
	got := cfg.Normalized()
	if got.ChannelID != "C0123456789" {
		t.Fatalf("normalized channel = %q", got.ChannelID)
	}
	if got.CaseFileBaseURL != "https://cases.example.test/v1/cases" {
		t.Fatalf("normalized case URL = %q", got.CaseFileBaseURL)
	}
	if got := (SlackConfig{CaseFileBaseURL: "https://cases.example.test/"}).Normalized(); got.CaseFileBaseURL != "https://cases.example.test" {
		t.Fatalf("root trailing slash normalized to %q", got.CaseFileBaseURL)
	}

	// Invalid values are left intact so Validate reports the original problem.
	invalid := SlackConfig{CaseFileBaseURL: "ftp://cases.example.test"}
	if got := invalid.Normalized(); got.CaseFileBaseURL != invalid.CaseFileBaseURL {
		t.Fatalf("invalid URL was rewritten to %q", got.CaseFileBaseURL)
	}
}

func TestLoadSlackConfigFromYAMLAndEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvSlackBotToken, "bot-token-value")
	t.Setenv(EnvSlackSigningSecret, "signing-secret-value")

	path := writeSlackConfigFile(t, `slack:
  channel_id: C0123456789
  case_file_base_url: https://cases.example.test/
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, want := cfg.Slack.BotToken, "bot-token-value"; got != want {
		t.Errorf("Slack.BotToken = %q, want %q", got, want)
	}
	if got, want := cfg.Slack.SigningSecret, "signing-secret-value"; got != want {
		t.Errorf("Slack.SigningSecret = %q, want %q", got, want)
	}
	if got, want := cfg.Slack.ChannelID, "C0123456789"; got != want {
		t.Errorf("Slack.ChannelID = %q, want %q", got, want)
	}
	if got, want := cfg.Slack.CaseFileBaseURL, "https://cases.example.test"; got != want {
		t.Errorf("Slack.CaseFileBaseURL = %q, want normalized %q", got, want)
	}

	// Environment values override the non-secret YAML fields.
	t.Setenv(EnvSlackChannelID, "C9999999999")
	t.Setenv(EnvSlackCaseFileBaseURL, "https://env.example.test/")
	cfg, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile with env overrides: %v", err)
	}
	if got, want := cfg.Slack.ChannelID, "C9999999999"; got != want {
		t.Errorf("env Slack.ChannelID = %q, want %q", got, want)
	}
	if got, want := cfg.Slack.CaseFileBaseURL, "https://env.example.test"; got != want {
		t.Errorf("env Slack.CaseFileBaseURL = %q, want %q", got, want)
	}
}

func TestLoadRejectsSlackSecretsInYAML(t *testing.T) {
	clearEnv(t)
	for _, key := range []string{"bot_token", "signing_secret"} {
		t.Run(key, func(t *testing.T) {
			const leaked = "yaml-secret-value-must-not-leak"
			path := writeSlackConfigFile(t, "slack:\n  "+key+": "+leaked+"\n")
			_, err := LoadFile(path)
			if err == nil {
				t.Fatalf("LoadFile accepted slack.%s in YAML", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v, want it to name slack.%s", err, key)
			}
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaked the YAML secret: %v", err)
			}
		})
	}
}

func TestValidateAPIRuntimeRequiresSlackSecrets(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.AppURL = "postgres://app:app@localhost:5432/recon?sslmode=disable"
	cfg.API.Keys = []APIKey{{ID: "dev", Role: RoleAdmin}}
	cfg.Slack.SigningSecret = "super-secret-signing-value"

	// The API needs both secrets; the channel ID is outbox-only.
	if err := cfg.ValidateAPIRuntime(); err == nil || !strings.Contains(err.Error(), "slack.bot_token") {
		t.Fatalf("missing bot token error = %v, want slack.bot_token", err)
	} else if strings.Contains(err.Error(), cfg.Slack.SigningSecret) {
		t.Fatalf("error leaked the signing secret: %v", err)
	}

	cfg.Slack.BotToken = "super-secret-bot-token"
	cfg.Slack.SigningSecret = ""
	if err := cfg.ValidateAPIRuntime(); err == nil || !strings.Contains(err.Error(), "slack.signing_secret") {
		t.Fatalf("missing signing secret error = %v, want slack.signing_secret", err)
	} else if strings.Contains(err.Error(), cfg.Slack.BotToken) {
		t.Fatalf("error leaked the bot token: %v", err)
	}

	cfg.Slack.SigningSecret = "signing-secret-value"
	if err := cfg.ValidateAPIRuntime(); err != nil {
		t.Fatalf("ValidateAPIRuntime with both secrets and no channel ID: %v", err)
	}
}

func TestValidateOutboxRuntime(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.AppURL = "postgres://app:app@localhost:5432/recon?sslmode=disable"
	cfg.Slack = SlackConfig{
		BotToken:        "bot-token-value",
		ChannelID:       "C0123456789",
		CaseFileBaseURL: "https://cases.example.test",
	}
	if err := cfg.ValidateOutboxRuntime(); err != nil {
		t.Fatalf("ValidateOutboxRuntime: %v", err)
	}
	if cfg.Database.AgentURL != "" || cfg.LLM.APIKey != "" {
		t.Fatal("outbox runtime unexpectedly needs agent/LLM configuration")
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
		secret string
	}{
		{"missing app url", func(c *Config) { c.Database.AppURL = "" }, "database.app_url", ""},
		{"missing bot token", func(c *Config) { c.Slack.BotToken = "" }, "slack.bot_token", ""},
		{"missing channel id", func(c *Config) { c.Slack.ChannelID = "" }, "slack.channel_id", ""},
		{"missing case url", func(c *Config) { c.Slack.CaseFileBaseURL = "" }, "slack.case_file_base_url", ""},
		{"invalid case url", func(c *Config) { c.Slack.CaseFileBaseURL = "not-a-url" }, "slack.case_file_base_url", ""},
		{"whitespace channel", func(c *Config) { c.Slack.ChannelID = "  " }, "slack.channel_id", ""},
		{"secret not echoed", func(c *Config) { c.Slack.BotToken = "" }, "slack.bot_token", "bot-token-value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := cfg
			test.mutate(&bad)
			err := bad.ValidateOutboxRuntime()
			if err == nil {
				t.Fatal("ValidateOutboxRuntime accepted an incomplete config")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
			if test.secret != "" && strings.Contains(err.Error(), test.secret) {
				t.Fatalf("error leaked a secret: %v", err)
			}
		})
	}
}

func TestConfigValidateIncludesSlack(t *testing.T) {
	cfg := defaults()
	cfg.Database.URL = "postgres://recon:recon@localhost:5432/recon?sslmode=disable"
	cfg.Slack.ChannelID = "  "
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "slack.channel_id") {
		t.Fatalf("Config.Validate error = %v, want slack.channel_id", err)
	}
}

func TestSlackConfigFormattingRedactsSecrets(t *testing.T) {
	const (
		botToken      = "xoxb-super-secret-bot-token"
		signingSecret = "super-secret-signing-secret"
	)
	slackCfg := SlackConfig{
		BotToken:        botToken,
		SigningSecret:   signingSecret,
		ChannelID:       "C0123456789",
		CaseFileBaseURL: "https://cases.example.test",
	}
	cfg := defaults()
	cfg.Database.URL = "postgres://user:db-secret@localhost:5432/recon"
	cfg.Slack = slackCfg

	values := map[string]any{
		"slack value":    slackCfg,
		"slack pointer":  &slackCfg,
		"slack slice":    []SlackConfig{slackCfg},
		"config value":   cfg,
		"config pointer": &cfg,
	}
	for name, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(format, value)
			for _, secret := range []string{botToken, signingSecret, "db-secret"} {
				if strings.Contains(out, secret) {
					t.Fatalf("%s %s leaked %q: %s", name, format, secret, out)
				}
			}
		}
	}
	for name, out := range map[string]string{
		"SlackConfig.GoString": slackCfg.GoString(),
		"Config.String":        cfg.String(),
	} {
		for _, secret := range []string{botToken, signingSecret, "db-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, out)
			}
		}
	}
	if !strings.Contains(cfg.String(), "ChannelID:\"C0123456789\"") {
		t.Fatalf("Config.String hid the non-secret channel ID: %s", cfg.String())
	}
}

// TestSlackConfigJSONAndSlogRedactSecrets covers encoding/json and slog's JSON
// handler, which ignore String/Format/GoString. A JSON-encoded Config reaches
// the nested Slack field through SlackConfig.MarshalJSON.
func TestSlackConfigJSONAndSlogRedactSecrets(t *testing.T) {
	slackCfg := SlackConfig{
		BotToken:        slackJSONTestBotToken,
		SigningSecret:   slackJSONTestSigningSecret,
		ChannelID:       "C0123456789",
		CaseFileBaseURL: "https://cases.example.test",
	}
	cfg := defaults()
	cfg.Slack = slackCfg

	targets := []struct {
		name  string
		value any
	}{
		{name: "slack value", value: slackCfg},
		{name: "slack pointer", value: &slackCfg},
		{name: "config value", value: cfg},
		{name: "config pointer", value: &cfg},
	}
	for _, target := range targets {
		t.Run(target.name+"/json", func(t *testing.T) {
			encoded, err := json.Marshal(target.value)
			if err != nil {
				t.Fatalf("json.Marshal(%s): %v", target.name, err)
			}
			out := string(encoded)
			assertNoSlackSecrets(t, "json.Marshal("+target.name+")", out)
			assertSlackNonSecretsVisible(t, "json.Marshal("+target.name+")", out)
		})
		t.Run(target.name+"/slog-json", func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).
				Info("config", slog.Any("value", target.value))
			out := buf.String()
			assertNoSlackSecrets(t, "slog JSON handler("+target.name+")", out)
			assertSlackNonSecretsVisible(t, "slog JSON handler("+target.name+")", out)
			for _, key := range []string{"bot_token", "signing_secret"} {
				if !strings.Contains(out, `"`+key+`"`) {
					t.Fatalf("slog JSON handler(%s) omitted %q: %s", target.name, key, out)
				}
			}
		})
	}
}

func assertNoSlackSecrets(t *testing.T, label, out string) {
	t.Helper()
	for _, forbidden := range []string{
		slackJSONTestBotToken,
		slackJSONTestSigningSecret,
		"Bearer",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%s leaked %q: %s", label, forbidden, out)
		}
	}
}

func assertSlackNonSecretsVisible(t *testing.T, label, out string) {
	t.Helper()
	for _, want := range []string{"C0123456789", "https://cases.example.test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s hid the non-secret %q: %s", label, want, out)
		}
	}
}
