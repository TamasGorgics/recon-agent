package slack

import (
	"encoding/json"
	"log/slog"
	"time"
)

// ClientOptions holds the bot token directly. String/GoString/Format cover fmt
// verbs, but encoding/json and slog's JSON handler ignore those methods and
// would otherwise serialize the exported BotToken field. LogValue and
// MarshalJSON close that path; both are value methods so pointers inherit them.

// LogValue implements slog.LogValuer. A JSON or text handler resolves it
// before appending the attribute, so slog.Any("options", opts) renders the
// redacted group instead of the struct fields.
func (o ClientOptions) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", o.BaseURL),
		slog.String("bot_token", redactedSecret),
		slog.Duration("timeout", o.Timeout),
		slog.Int("max_attempts", o.MaxAttempts),
		slog.Duration("max_retry_wait", o.MaxRetryWait),
	)
}

// MarshalJSON implements json.Marshaler for direct encoding/json callers and
// for any parent struct that is marshaled. HTTPClient, Clock, and Sleeper are
// omitted: they are injected dependencies, not configuration, and have no
// stable JSON shape. The non-secret options stay visible for diagnostics.
func (o ClientOptions) MarshalJSON() ([]byte, error) {
	type clientOptionsJSON struct {
		BaseURL      string        `json:"base_url"`
		BotToken     string        `json:"bot_token"`
		Timeout      time.Duration `json:"timeout"`
		MaxAttempts  int           `json:"max_attempts"`
		MaxRetryWait time.Duration `json:"max_retry_wait"`
	}
	return json.Marshal(clientOptionsJSON{
		BaseURL:      o.BaseURL,
		BotToken:     redactedSecret,
		Timeout:      o.Timeout,
		MaxAttempts:  o.MaxAttempts,
		MaxRetryWait: o.MaxRetryWait,
	})
}
