package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// CassetteMode selects record, replay, or passthrough behavior.
type CassetteMode string

const (
	CassetteRecord      CassetteMode = "record"
	CassetteReplay      CassetteMode = "replay"
	CassettePassthrough CassetteMode = "passthrough"
)

const (
	cassetteSchema     = 1
	DefaultCassetteDir = "eval/cassettes"
	maxCassetteBytes   = 1 << 20
)

var cassetteComponentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// CassetteOptions configures a Cassette client. Now supplies the recording
// timestamp only; replay never reads the clock.
type CassetteOptions struct {
	Mode     CassetteMode
	Dir      string
	Update   bool
	Provider string
	BaseURL  string
	Now      func() time.Time
}

// Cassette wraps any Client with deterministic record/replay.
type Cassette struct {
	inner    Client
	mode     CassetteMode
	dir      string
	update   bool
	provider string
	baseURL  string
	now      func() time.Time
}

// NewCassette validates options and returns a cassette wrapper. Replay allows a
// nil inner client because it never calls through.
func NewCassette(inner Client, opts CassetteOptions) (*Cassette, error) {
	switch opts.Mode {
	case CassetteRecord, CassetteReplay, CassettePassthrough:
	default:
		return nil, fmt.Errorf("llm: invalid cassette mode %q", opts.Mode)
	}
	if opts.Mode != CassetteReplay && inner == nil {
		return nil, errors.New("llm: cassette requires an inner client")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = DefaultCassetteDir
	}
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" {
		provider = ProviderOpenCodeGo
	}
	if err := validateCassetteComponent(provider); err != nil {
		return nil, fmt.Errorf("llm: cassette provider: %w", err)
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Cassette{
		inner:    inner,
		mode:     opts.Mode,
		dir:      dir,
		update:   opts.Update,
		provider: provider,
		baseURL:  baseURL,
		now:      now,
	}, nil
}

// Complete records, replays, or passes through one request.
func (c *Cassette) Complete(ctx context.Context, req Request) (Response, error) {
	switch c.mode {
	case CassettePassthrough:
		return c.inner.Complete(ctx, req)
	case CassetteReplay:
		return c.replay(req)
	case CassetteRecord:
		response, err := c.inner.Complete(ctx, req)
		if err != nil {
			return Response{}, err
		}
		response, err = normalizeCassetteResponse(response)
		if err != nil {
			return Response{}, fmt.Errorf("llm: normalize cassette response: %w", err)
		}
		response, err = c.record(req, response)
		if err != nil {
			return Response{}, err
		}
		return response, nil
	default:
		return Response{}, fmt.Errorf("llm: invalid cassette mode %q", c.mode)
	}
}

type canonicalRequest struct {
	System      string      `json:"system"`
	Messages    []Message   `json:"messages"`
	Tools       []ToolDef   `json:"tools"`
	ToolChoice  *ToolChoice `json:"tool_choice"`
	Temperature *float64    `json:"temperature"` //floatlint:allow provider temperature, not money
	MaxTokens   int         `json:"max_tokens"`
	Stream      bool        `json:"stream"`
}

type canonicalKey struct {
	CassetteSchema int              `json:"cassette_schema"`
	Provider       string           `json:"provider"`
	BaseURL        string           `json:"base_url"`
	Model          string           `json:"model"`
	PromptVersion  string           `json:"prompt_version"`
	Request        canonicalRequest `json:"request"`
}

type cassetteFile struct {
	Schema         int              `json:"schema"`
	Key            string           `json:"key"`
	RecordedAt     string           `json:"recorded_at"`
	Provider       string           `json:"provider"`
	BaseURL        string           `json:"base_url"`
	PromptVersion  string           `json:"prompt_version"`
	RequestedModel string           `json:"requested_model"`
	ActualModel    string           `json:"actual_model"`
	Request        canonicalRequest `json:"request"`
	Response       Response         `json:"response"`
	DurationMS     int64            `json:"duration_ms"`
	Redactions     []string         `json:"redactions"`
}

func requestFor(req Request) canonicalRequest {
	messages := append([]Message(nil), req.Messages...)
	if messages == nil {
		messages = []Message{}
	}
	tools := append([]ToolDef(nil), req.Tools...)
	if tools == nil {
		tools = []ToolDef{}
	}
	return canonicalRequest{
		System:      req.System,
		Messages:    messages,
		Tools:       tools,
		ToolChoice:  effectiveToolChoice(req),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
}

// CanonicalRequestHash returns the SHA-256 key used by both the cassette layer
// and the agent's llm_calls.request_hash column. The provider and base URL are
// part of the document so a request cannot silently replay against a different
// endpoint.
func CanonicalRequestHash(provider, baseURL string, req Request) (string, error) {
	if err := req.Validate(); err != nil {
		return "", newRequestError(provider, APIID(req.Model), err.Error())
	}
	if strings.TrimSpace(req.PromptVersion) == "" {
		return "", newRequestError(provider, APIID(req.Model), "prompt version is required for cassettes")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	doc := canonicalKey{
		CassetteSchema: cassetteSchema,
		Provider:       provider,
		BaseURL:        strings.TrimRight(baseURL, "/"),
		Model:          APIID(req.Model),
		PromptVersion:  req.PromptVersion,
		Request:        requestFor(req),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("llm: canonical request hash: %w", err)
	}
	canonical, err := canonicalizeJSON(raw)
	if err != nil {
		return "", fmt.Errorf("llm: canonical request hash: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Cassette) key(req Request) (string, error) {
	return CanonicalRequestHash(c.provider, c.baseURL, req)
}

func canonicalizeJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func (c *Cassette) path(req Request, key string) (string, error) {
	model := APIID(req.Model)
	for name, value := range map[string]string{
		"model":          model,
		"prompt_version": req.PromptVersion,
	} {
		if err := validateCassetteComponent(value); err != nil {
			return "", fmt.Errorf("llm: cassette %s: %w", name, err)
		}
	}
	return filepath.Join(c.dir, c.provider, model, req.PromptVersion, key+".json"), nil
}

func validateCassetteComponent(value string) error {
	if value == "" {
		return errors.New("empty path component")
	}
	if value == "." || value == ".." || !cassetteComponentPattern.MatchString(value) {
		return fmt.Errorf("unsafe path component %q", value)
	}
	return nil
}

// record writes the cassette and returns the response exactly as replay will
// read it. Redaction happens before the file is written, so a recorded response
// and its replay remain deep-equal even when content contained a secret.
func (c *Cassette) record(req Request, response Response) (Response, error) {
	key, err := c.key(req)
	if err != nil {
		return Response{}, err
	}
	path, err := c.path(req, key)
	if err != nil {
		return Response{}, err
	}
	file := cassetteFile{
		Schema:         cassetteSchema,
		Key:            key,
		RecordedAt:     c.now().UTC().Format(time.RFC3339),
		Provider:       c.provider,
		BaseURL:        c.baseURL,
		PromptVersion:  req.PromptVersion,
		RequestedModel: APIID(req.Model),
		ActualModel:    response.ActualModel,
		Request:        requestFor(req),
		Response:       response,
		DurationMS:     response.Latency.Milliseconds(),
	}
	probe, err := json.Marshal(file)
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal cassette: %w", err)
	}
	_, classes := RedactWithClasses(string(probe))
	if classes == nil {
		classes = []string{}
	}
	file.Redactions = classes
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal cassette: %w", err)
	}
	raw = append(raw, '\n')
	raw = RedactBytes(raw)
	if len(raw) > maxCassetteBytes {
		return Response{}, fmt.Errorf("llm: cassette %s exceeds %d bytes", path, maxCassetteBytes)
	}

	var stored cassetteFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Response{}, fmt.Errorf("llm: decode redacted cassette: %w", err)
	}
	storedResponse, err := normalizeCassetteResponse(stored.Response)
	if err != nil {
		return Response{}, fmt.Errorf("llm: normalize redacted cassette response: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Response{}, fmt.Errorf("llm: create cassette directory: %w", err)
	}
	if !c.update {
		if _, err := os.Stat(path); err == nil {
			return Response{}, fmt.Errorf("llm: cassette %s already exists (set Update to overwrite)", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Response{}, fmt.Errorf("llm: stat cassette: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".cassette-*")
	if err != nil {
		return Response{}, fmt.Errorf("llm: create cassette temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return Response{}, fmt.Errorf("llm: chmod cassette: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return Response{}, fmt.Errorf("llm: write cassette: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Response{}, fmt.Errorf("llm: close cassette: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Response{}, fmt.Errorf("llm: publish cassette: %w", err)
	}
	return storedResponse, nil
}

func (c *Cassette) replay(req Request) (Response, error) {
	key, err := c.key(req)
	if err != nil {
		return Response{}, err
	}
	path, err := c.path(req, key)
	if err != nil {
		return Response{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette not found", err)
	}
	if len(data) > maxCassetteBytes {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette exceeds size cap", nil)
	}
	var file cassetteFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&file); err != nil {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette is corrupt", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette has trailing data", nil)
	}
	if file.Schema != cassetteSchema {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette schema mismatch", nil)
	}
	if file.Key != key {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette key mismatch", nil)
	}
	response, err := normalizeCassetteResponse(file.Response)
	if err != nil {
		return Response{}, cassetteMiss(c.provider, req.Model, "cassette response is corrupt", err)
	}
	return response, nil
}

// normalizeCassetteResponse compacts raw JSON so pretty-printing a cassette
// file cannot change the replayed response bytes.
func normalizeCassetteResponse(response Response) (Response, error) {
	raw, err := compactRaw(response.Raw)
	if err != nil {
		return Response{}, err
	}
	usageRaw, err := compactRaw(response.Usage.Raw)
	if err != nil {
		return Response{}, err
	}
	response.Raw = raw
	response.Usage.Raw = usageRaw
	// JSON represents every zero decimal as "0"; canonicalize zero values so a
	// recorded response and its replay are structurally identical.
	for _, value := range []*decimal.Decimal{
		&response.Usage.APICost,
		&response.Usage.ListCost,
		&response.Usage.ComputedCost,
		&response.Usage.CostUSD,
		&response.Usage.Allowance,
	} {
		if value.IsZero() {
			*value = decimal.Zero
		}
	}
	return response, nil
}

func compactRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), out.Bytes()...), nil
}

func cassetteMiss(provider, model, message string, cause error) *ProviderError {
	wrapped := ErrCassetteMiss
	if cause != nil {
		wrapped = fmt.Errorf("%w: %v", ErrCassetteMiss, cause)
	}
	return &ProviderError{
		Provider:  provider,
		Model:     APIID(model),
		Kind:      KindCassetteMiss,
		Retryable: false,
		Message:   Redact(message),
		Cause:     wrapped,
	}
}
