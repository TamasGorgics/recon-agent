package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// Web API defaults. A caller may override attempts and the total wait budget;
// the per-request HTTP timeout belongs to the injected *http.Client.
const (
	DefaultBaseURL      = "https://slack.com/api"
	DefaultMaxAttempts  = 3
	DefaultMaxRetryWait = 10 * time.Second
	DefaultHTTPTimeout  = 10 * time.Second
	defaultRetryAfter   = time.Second
	maxResponseBytes    = 1 << 20
)

// Public typed errors. Rate-limit and availability errors wrap these
// sentinels, so both errors.Is and errors.As work for callers and tests.
var (
	ErrRateLimited      = errors.New("approval/slack: rate limited")
	ErrSlackUnavailable = errors.New("approval/slack: slack unavailable")
	ErrSlackResponse    = errors.New("approval/slack: invalid Slack response")
	ErrInvalidClient    = errors.New("approval/slack: invalid Slack client")
)

// RateLimitedError reports an HTTP 429 that could not be retried within the
// configured attempt/wait budget. RetryAfter is Slack's requested delay.
type RateLimitedError struct {
	RetryAfter time.Duration
	Attempts   int
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("approval/slack: rate limited (retry after %s after %d attempt(s))",
		e.RetryAfter, e.Attempts)
}

func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// UnavailableError reports exhausted retries for HTTP 5xx or transport
// failures. It never contains the bot token, request body, or response body.
type UnavailableError struct {
	Status   int
	Attempts int
	Cause    error
}

func (e *UnavailableError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("approval/slack: slack unavailable (HTTP %d after %d attempt(s))",
			e.Status, e.Attempts)
	}
	return fmt.Sprintf("approval/slack: slack unavailable (transport error after %d attempt(s))",
		e.Attempts)
}

func (e *UnavailableError) Unwrap() error { return e.Cause }

// Is lets errors.Is(err, ErrSlackUnavailable) work while Unwrap preserves the
// underlying transport cause.
func (e *UnavailableError) Is(target error) bool { return target == ErrSlackUnavailable }

// ErrSlackAPI is Slack's application-level {"ok":false,"error":"code"}. The
// code is the only server-provided text kept.
type ErrSlackAPI struct {
	Code string
}

func (e ErrSlackAPI) Error() string {
	return "approval/slack: slack api error: " + safeCode(e.Code)
}

// HTTPError is a non-2xx, non-429, non-5xx Slack response. The body is never
// included; only the status and Slack error code are retained.
type HTTPError struct {
	Method string
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("approval/slack: %s HTTP %d", e.Method, e.Status)
	}
	return fmt.Sprintf("approval/slack: %s HTTP %d: %s", e.Method, e.Status, safeCode(e.Code))
}

// Sleeper pauses a retry. It is injectable so rate-limit and 5xx tests are
// deterministic and never sleep on the wall clock.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// RealSleeper is the production Sleeper.
type RealSleeper struct{}

// Sleep waits for d or until ctx is done, whichever happens first.
func (RealSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ClientOptions configures a Slack Web API client. Zero values select the
// defaults documented above. BotToken is required and is never rendered in an
// error or log line.
type ClientOptions struct {
	BaseURL      string
	BotToken     string
	HTTPClient   *http.Client
	Timeout      time.Duration
	Clock        clock.Clock
	Sleeper      Sleeper
	MaxAttempts  int
	MaxRetryWait time.Duration
}

// Client is an immutable Slack Web API client. It is safe for concurrent use
// after construction.
type Client struct {
	baseURL      string
	botToken     string
	httpClient   *http.Client
	clock        clock.Clock
	sleeper      Sleeper
	maxAttempts  int
	maxRetryWait time.Duration
}

// NewClient validates options and returns a client. HTTPClient nil selects a
// client with Timeout (default 10 s) as its per-request timeout. MaxAttempts
// includes the first attempt, so the default 3 means at most two retries.
func NewClient(opts ClientOptions) (*Client, error) {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Opaque != "" ||
		(!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) ||
		parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("%w: base URL must be an absolute http or https URL without userinfo, query, or fragment",
			ErrInvalidClient)
	}
	if strings.TrimSpace(opts.BotToken) == "" {
		return nil, fmt.Errorf("%w: bot token is required", ErrInvalidClient)
	}
	if strings.TrimSpace(opts.BotToken) != opts.BotToken {
		return nil, fmt.Errorf("%w: bot token has surrounding whitespace", ErrInvalidClient)
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if maxAttempts < 1 {
		return nil, fmt.Errorf("%w: max attempts must be at least 1, got %d",
			ErrInvalidClient, maxAttempts)
	}
	maxRetryWait := opts.MaxRetryWait
	if maxRetryWait == 0 {
		maxRetryWait = DefaultMaxRetryWait
	}
	if maxRetryWait < 0 {
		return nil, fmt.Errorf("%w: max retry wait must not be negative, got %s",
			ErrInvalidClient, maxRetryWait)
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultHTTPTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("%w: HTTP timeout must not be negative, got %s",
			ErrInvalidClient, timeout)
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	timeSource := opts.Clock
	if timeSource == nil {
		timeSource = clock.Real{}
	}
	sleeper := opts.Sleeper
	if sleeper == nil {
		sleeper = RealSleeper{}
	}

	return &Client{
		baseURL:      strings.TrimRight(base, "/"),
		botToken:     opts.BotToken,
		httpClient:   httpClient,
		clock:        timeSource,
		sleeper:      sleeper,
		maxAttempts:  maxAttempts,
		maxRetryWait: maxRetryWait,
	}, nil
}

// PostMessage sends chat.postMessage and returns Slack's message timestamp.
// msg.ClientMsgID is required and is sent as client_msg_id (the proposal ID)
// for best-effort duplicate suppression.
func (c *Client) PostMessage(ctx context.Context, channel string, msg OutgoingMessage) (string, error) {
	response, err := c.postMessage(ctx, channel, msg)
	if err != nil {
		return "", err
	}
	return response.TS, nil
}

// PostMessageRef is PostMessage plus the frozen external-ref form
// "<channel>:<ts>". Slack's response channel is preferred when present;
// otherwise the requested channel is used. No other package needs to know the
// response-channel fallback.
func (c *Client) PostMessageRef(ctx context.Context, channel string, msg OutgoingMessage) (string, error) {
	response, err := c.postMessage(ctx, channel, msg)
	if err != nil {
		return "", err
	}
	refChannel := strings.TrimSpace(response.Channel)
	if refChannel == "" {
		refChannel = channel
	}
	return refChannel + ":" + response.TS, nil
}

func (c *Client) postMessage(ctx context.Context, channel string, msg OutgoingMessage) (apiResponse, error) {
	if err := validateChannel(channel); err != nil {
		return apiResponse{}, err
	}
	if err := validateOutgoingMessage(msg); err != nil {
		return apiResponse{}, err
	}
	if msg.ClientMsgID == "" {
		return apiResponse{}, fmt.Errorf("%w: client_msg_id is required for chat.postMessage",
			ErrInvalidClient)
	}
	if !canonicalUUIDPattern.MatchString(msg.ClientMsgID) {
		return apiResponse{}, fmt.Errorf("%w: client_msg_id %q is not a canonical lowercase UUID",
			ErrInvalidClient, msg.ClientMsgID)
	}
	response, err := c.call(ctx, "chat.postMessage", postMessagePayload{
		Channel:     channel,
		Text:        msg.Text,
		Blocks:      msg.Blocks,
		ClientMsgID: msg.ClientMsgID,
	})
	if err != nil {
		return apiResponse{}, err
	}
	if strings.TrimSpace(response.TS) == "" {
		return apiResponse{}, fmt.Errorf("%w: chat.postMessage returned no ts", ErrSlackResponse)
	}
	return response, nil
}

// UpdateMessage sends chat.update for an existing channel/ts. It never sends
// client_msg_id; Slack only accepts that on the original post.
func (c *Client) UpdateMessage(ctx context.Context, channel, ts string, msg OutgoingMessage) error {
	if err := validateChannel(channel); err != nil {
		return err
	}
	if strings.TrimSpace(ts) == "" || strings.TrimSpace(ts) != ts {
		return fmt.Errorf("%w: message ts is required and must not have surrounding whitespace",
			ErrInvalidClient)
	}
	if err := validateOutgoingMessage(msg); err != nil {
		return err
	}
	_, err := c.call(ctx, "chat.update", updateMessagePayload{
		Channel: channel,
		TS:      ts,
		Text:    msg.Text,
		Blocks:  msg.Blocks,
	})
	return err
}

// PostEphemeral sends chat.postEphemeral with plain text only. It is used for
// denial/stale notices and intentionally has no Block Kit parameter.
func (c *Client) PostEphemeral(ctx context.Context, channel, user, text string) error {
	if err := validateChannel(channel); err != nil {
		return err
	}
	if strings.TrimSpace(user) == "" || strings.TrimSpace(user) != user {
		return fmt.Errorf("%w: user id is required and must not have surrounding whitespace",
			ErrInvalidClient)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: ephemeral text is required", ErrInvalidClient)
	}
	_, err := c.call(ctx, "chat.postEphemeral", ephemeralPayload{
		Channel: channel,
		User:    user,
		Text:    text,
	})
	return err
}

type postMessagePayload struct {
	Channel     string  `json:"channel"`
	Text        string  `json:"text"`
	Blocks      []Block `json:"blocks,omitempty"`
	ClientMsgID string  `json:"client_msg_id,omitempty"`
}

type updateMessagePayload struct {
	Channel string  `json:"channel"`
	TS      string  `json:"ts"`
	Text    string  `json:"text"`
	Blocks  []Block `json:"blocks,omitempty"`
}

type ephemeralPayload struct {
	Channel string `json:"channel"`
	User    string `json:"user"`
	Text    string `json:"text"`
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	TS      string `json:"ts"`
	Channel string `json:"channel"`
}

// Internal attempt classifications. They carry just enough for the retry loop
// and are converted to the public typed errors at the loop boundary.
type rateLimitedAttempt struct {
	retryAfter time.Duration
}

func (e *rateLimitedAttempt) Error() string { return "slack: rate limited" }

type unavailableAttempt struct {
	status int
	cause  error
}

func (e *unavailableAttempt) Error() string { return "slack: unavailable" }

func (c *Client) call(ctx context.Context, method string, payload any) (apiResponse, error) {
	if err := ctx.Err(); err != nil {
		return apiResponse{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return apiResponse{}, fmt.Errorf("%w: marshal %s request: %v",
			ErrInvalidClient, method, err)
	}

	start := c.clock.Now()
	var plannedWait time.Duration
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		response, err := c.attempt(ctx, method, encoded)
		if err == nil {
			return response, nil
		}

		var rateLimited *rateLimitedAttempt
		if errors.As(err, &rateLimited) {
			if attempt == c.maxAttempts {
				return apiResponse{}, &RateLimitedError{
					RetryAfter: rateLimited.retryAfter,
					Attempts:   attempt,
				}
			}
			if !c.waitFits(plannedWait, c.clock.Since(start), rateLimited.retryAfter) {
				return apiResponse{}, &RateLimitedError{
					RetryAfter: rateLimited.retryAfter,
					Attempts:   attempt,
				}
			}
			if sleepErr := c.sleeper.Sleep(ctx, rateLimited.retryAfter); sleepErr != nil {
				return apiResponse{}, sleepFailure(ctx, sleepErr)
			}
			plannedWait += rateLimited.retryAfter
			continue
		}

		var unavailable *unavailableAttempt
		if errors.As(err, &unavailable) {
			if attempt == c.maxAttempts {
				return apiResponse{}, &UnavailableError{
					Status:   unavailable.status,
					Attempts: attempt,
					Cause:    unavailable.cause,
				}
			}
			delay := retryBackoff(attempt)
			if !c.waitFits(plannedWait, c.clock.Since(start), delay) {
				return apiResponse{}, &UnavailableError{
					Status:   unavailable.status,
					Attempts: attempt,
					Cause:    unavailable.cause,
				}
			}
			if sleepErr := c.sleeper.Sleep(ctx, delay); sleepErr != nil {
				return apiResponse{}, sleepFailure(ctx, sleepErr)
			}
			plannedWait += delay
			continue
		}

		// Application errors, unexpected 4xx, malformed responses: no retry.
		return apiResponse{}, err
	}

	return apiResponse{}, &UnavailableError{Attempts: c.maxAttempts}
}

// waitFits reports whether one more delay keeps planned/elapsed wait inside
// the budget. It uses the larger of the planned waits and the injected clock's
// elapsed time so a fake clock and a fake sleeper both remain deterministic.
func (c *Client) waitFits(plannedWait, elapsed, delay time.Duration) bool {
	used := plannedWait
	if elapsed > used {
		used = elapsed
	}
	if delay > c.maxRetryWait {
		return false
	}
	return used+delay <= c.maxRetryWait
}

func (c *Client) attempt(ctx context.Context, method string, encoded []byte) (apiResponse, error) {
	endpoint := c.baseURL + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return apiResponse{}, fmt.Errorf("%w: build %s request: %v", ErrInvalidClient, method, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.botToken)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")

	httpResponse, err := c.httpClient.Do(request)
	if err != nil {
		return apiResponse{}, &unavailableAttempt{cause: err}
	}
	defer func() { _ = httpResponse.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, maxResponseBytes+1))
	if readErr != nil {
		return apiResponse{}, &unavailableAttempt{
			status: httpResponse.StatusCode,
			cause:  readErr,
		}
	}
	if len(raw) > maxResponseBytes {
		return apiResponse{}, &HTTPError{
			Method: method,
			Status: httpResponse.StatusCode,
			Code:   "response_too_large",
		}
	}

	if httpResponse.StatusCode == http.StatusTooManyRequests {
		return apiResponse{}, &rateLimitedAttempt{
			retryAfter: parseRetryAfter(httpResponse.Header.Get("Retry-After")),
		}
	}
	if httpResponse.StatusCode >= 500 {
		return apiResponse{}, &unavailableAttempt{status: httpResponse.StatusCode}
	}

	var decoded apiResponse
	decodeErr := json.Unmarshal(raw, &decoded)
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		code := ""
		if decodeErr == nil {
			code = decoded.Error
		}
		return apiResponse{}, &HTTPError{
			Method: method,
			Status: httpResponse.StatusCode,
			Code:   code,
		}
	}
	if decodeErr != nil {
		return apiResponse{}, fmt.Errorf("%w: %s returned a non-JSON body", ErrSlackResponse, method)
	}
	if !decoded.OK {
		return apiResponse{}, ErrSlackAPI{Code: safeCode(decoded.Error)}
	}
	return decoded, nil
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return defaultRetryAfter
	}
	// Clamp absurd values so the duration multiplication cannot overflow and
	// so the retry loop reports a sane Retry-After to its caller.
	const maxRetryAfterSeconds = int64((24 * time.Hour) / time.Second)
	if seconds > maxRetryAfterSeconds {
		return 24 * time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Second
	for i := 1; i < attempt; i++ {
		if delay >= DefaultMaxRetryWait/2 {
			return DefaultMaxRetryWait
		}
		delay *= 2
	}
	return delay
}

func sleepFailure(ctx context.Context, sleepErr error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	return sleepErr
}

func validateChannel(channel string) error {
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("%w: channel id is required", ErrInvalidClient)
	}
	if strings.TrimSpace(channel) != channel {
		return fmt.Errorf("%w: channel id must not have surrounding whitespace", ErrInvalidClient)
	}
	if strings.Contains(channel, ":") {
		return fmt.Errorf("%w: channel id must not contain a colon", ErrInvalidClient)
	}
	return nil
}

func validateOutgoingMessage(msg OutgoingMessage) error {
	if strings.TrimSpace(msg.Text) == "" && len(msg.Blocks) == 0 {
		return fmt.Errorf("%w: message requires text or blocks", ErrInvalidClient)
	}
	return nil
}

// safeCode keeps error codes short and printable. It never echoes a token or
// payload, and a missing code becomes a stable placeholder.
func safeCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "unknown_error"
	}
	var b strings.Builder
	for _, r := range code {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if b.Len() >= 128 {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "unknown_error"
	}
	return b.String()
}
