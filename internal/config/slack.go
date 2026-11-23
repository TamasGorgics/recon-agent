package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Environment variable names for the Slack adapter. The bot token and signing
// secret are environment-only; the two non-secret fields may also come from
// the slack YAML block.
const (
	EnvSlackBotToken        = "SLACK_BOT_TOKEN"
	EnvSlackSigningSecret   = "SLACK_SIGNING_SECRET"
	EnvSlackChannelID       = "SLACK_CHANNEL_ID"
	EnvSlackCaseFileBaseURL = "SLACK_CASE_FILE_BASE_URL"
)

// SlackConfig configures the Slack approval adapter. BotToken and
// SigningSecret are yaml:"-" so a YAML secret is a strict-decode error; they
// are populated only from the environment.
type SlackConfig struct {
	BotToken        string `yaml:"-"`
	SigningSecret   string `yaml:"-"`
	ChannelID       string `yaml:"channel_id"`
	CaseFileBaseURL string `yaml:"case_file_base_url"`
}

// String renders the Slack block without either secret.
func (c SlackConfig) String() string {
	return fmt.Sprintf("SlackConfig{BotToken:%s, SigningSecret:%s, ChannelID:%q, CaseFileBaseURL:%q}",
		redactSecret(c.BotToken), redactSecret(c.SigningSecret), c.ChannelID, c.CaseFileBaseURL)
}

// Format covers every verb, including %#v, for both SlackConfig values and
// pointers. Secrets are rendered as <redacted> and are never echoed.
func (c SlackConfig) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// GoString keeps direct %#v-style callers from falling back to struct fields.
func (c SlackConfig) GoString() string { return c.String() }

// Validate checks the non-secret Slack fields when present. It deliberately
// does not require any field, so commands that never talk to Slack still
// validate. The API/outbox runtime validators add their required secrets and
// non-secret fields.
func (c SlackConfig) Validate() error {
	if c.ChannelID != "" {
		if strings.TrimSpace(c.ChannelID) == "" {
			return errors.New("config: slack.channel_id must be non-empty when set")
		}
		if strings.TrimSpace(c.ChannelID) != c.ChannelID {
			return errors.New("config: slack.channel_id must not have surrounding whitespace")
		}
	}
	if c.CaseFileBaseURL != "" {
		if _, err := normalizeSlackCaseFileBaseURL(c.CaseFileBaseURL); err != nil {
			return err
		}
	}
	return nil
}

// Normalized trims the non-secret fields and removes a trailing slash from the
// case-file base URL. An invalid URL is left unchanged so Validate reports it.
func (c SlackConfig) Normalized() SlackConfig {
	c.ChannelID = strings.TrimSpace(c.ChannelID)
	if c.CaseFileBaseURL != "" {
		if normalized, err := normalizeSlackCaseFileBaseURL(c.CaseFileBaseURL); err == nil {
			c.CaseFileBaseURL = normalized
		}
	}
	return c
}

// ValidateOutboxRuntime requires the configuration the outbox worker uses:
// the app DSN, the bot token, the channel ID, and the case-file base URL. The
// signing secret is API-only, and no agent DSN or LLM key is needed.
func (c Config) ValidateOutboxRuntime() error {
	if strings.TrimSpace(c.Database.AppURL) == "" {
		return errors.New("config: database.app_url is required for the outbox worker (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")
	}
	if err := c.Slack.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Slack.BotToken) == "" {
		return errors.New("config: slack.bot_token is required for the outbox worker (set SLACK_BOT_TOKEN)")
	}
	if strings.TrimSpace(c.Slack.ChannelID) == "" {
		return errors.New("config: slack.channel_id is required for the outbox worker (set SLACK_CHANNEL_ID or slack.channel_id in CONFIG_FILE)")
	}
	if strings.TrimSpace(c.Slack.CaseFileBaseURL) == "" {
		return errors.New("config: slack.case_file_base_url is required for the outbox worker (set SLACK_CASE_FILE_BASE_URL or slack.case_file_base_url in CONFIG_FILE)")
	}
	if err := c.Metrics.Validate(); err != nil {
		return err
	}
	return nil
}

// normalizeSlackCaseFileBaseURL accepts an absolute http/https URL with no
// userinfo, query, or fragment and returns it without a trailing slash. The
// generated case-file link is always built server-side from this value.
func normalizeSlackCaseFileBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("config: slack.case_file_base_url must be an absolute http or https URL without userinfo, query, or fragment")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("config: slack.case_file_base_url must be an absolute http or https URL without userinfo, query, or fragment")
	}
	if parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("config: slack.case_file_base_url must be an absolute http or https URL without userinfo, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}
