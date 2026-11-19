package config

import (
	"encoding/json"
	"log/slog"
)

// SlackConfig holds two environment-only secrets. String/GoString/Format cover
// fmt verbs, but encoding/json and slog's JSON handler ignore those methods and
// would otherwise serialize the exported fields. LogValue and MarshalJSON close
// that path without changing how the struct is loaded or validated.
//
// Config needs no separate marshaler: a JSON-encoded Config reaches the nested
// Slack field through SlackConfig.MarshalJSON, and slog.Any("config", cfg)
// falls back to encoding/json, which does the same.

// LogValue implements slog.LogValuer so every handler redacts both secrets.
func (c SlackConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("bot_token", redactSecret(c.BotToken)),
		slog.String("signing_secret", redactSecret(c.SigningSecret)),
		slog.String("channel_id", c.ChannelID),
		slog.String("case_file_base_url", c.CaseFileBaseURL),
	)
}

// MarshalJSON implements json.Marshaler with the same redaction rule as
// String: a non-empty secret becomes <redacted>, and the non-secret fields
// remain visible.
func (c SlackConfig) MarshalJSON() ([]byte, error) {
	type slackConfigJSON struct {
		BotToken        string `json:"bot_token"`
		SigningSecret   string `json:"signing_secret"`
		ChannelID       string `json:"channel_id"`
		CaseFileBaseURL string `json:"case_file_base_url"`
	}
	return json.Marshal(slackConfigJSON{
		BotToken:        redactSecret(c.BotToken),
		SigningSecret:   redactSecret(c.SigningSecret),
		ChannelID:       c.ChannelID,
		CaseFileBaseURL: c.CaseFileBaseURL,
	})
}
