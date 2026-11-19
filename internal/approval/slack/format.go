package slack

import (
	"fmt"
	"io"
)

// redactedSecret is the stable placeholder used by every formatter in this
// package. It is deliberately not derived from the secret, so no prefix leaks.
const redactedSecret = "<redacted>"

// String renders a Client without the bot token. The HTTP client, clock, and
// sleeper are omitted: they carry no useful identity and may have large or
// implementation-specific formatting.
func (c Client) String() string {
	return fmt.Sprintf("slack.Client{baseURL:%q, botToken:%s, maxAttempts:%d, maxRetryWait:%s}",
		c.baseURL, redactedSecret, c.maxAttempts, c.maxRetryWait)
}

// GoString keeps %#v from falling back to reflection over unexported fields.
func (c Client) GoString() string { return c.String() }

// Format covers every verb, including %v, %+v, %#v, and %s, for Client values
// and pointers. fmt prefers Formatter over Stringer/GoStringer.
func (c Client) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// String renders the handler dependency set without its Client token. Handler
// values have no Stringer of their own otherwise, so fmt would reflect into the
// unexported client field on %s and could expose the token.
func (h Handlers) String() string {
	return fmt.Sprintf("slack.Handlers{channelID:%q, client:%s}",
		h.channelID, formatClientToken(h.client))
}

func (h Handlers) GoString() string { return h.String() }

func (h Handlers) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, h.String())
}

// String renders the interaction handler without its Client token.
func (h InteractionHandler) String() string {
	return fmt.Sprintf("slack.InteractionHandler{client:%s}", formatClientToken(h.client))
}

func (h InteractionHandler) GoString() string { return h.String() }

func (h InteractionHandler) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, h.String())
}

// String renders the Channel without its Client token.
func (c Channel) String() string {
	return fmt.Sprintf("slack.Channel{channelID:%q, client:%s}",
		c.channelID, formatClientToken(c.client))
}

func (c Channel) GoString() string { return c.String() }

func (c Channel) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// ClientOptions is an input struct, not a live client, but it holds the token
// directly. Redact it too so a debug dump of the construction options cannot
// leak the secret.
func (o ClientOptions) String() string {
	return fmt.Sprintf("slack.ClientOptions{baseURL:%q, botToken:%s, maxAttempts:%d, maxRetryWait:%s}",
		o.BaseURL, redactedSecret, o.MaxAttempts, o.MaxRetryWait)
}

func (o ClientOptions) GoString() string { return o.String() }

func (o ClientOptions) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, o.String())
}

// formatClientToken formats the embedded client through its own Formatter and
// keeps a nil dependency readable.
func formatClientToken(c *Client) string {
	if c == nil {
		return "<nil>"
	}
	return c.String()
}
