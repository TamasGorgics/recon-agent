package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Role is an API principal's authorization role. The numeric order is also the
// privilege order: viewer < approver < admin.
type Role string

// The three API roles from spec section 15.
const (
	RoleViewer   Role = "viewer"
	RoleApprover Role = "approver"
	RoleAdmin    Role = "admin"
)

// Valid reports whether r is one of the configured API roles.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleApprover, RoleAdmin:
		return true
	default:
		return false
	}
}

// Rank returns the privilege rank of r: viewer=1, approver=2, admin=3. An
// unknown role ranks 0.
func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleApprover:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// Allows reports whether a principal with role r satisfies required.
func (r Role) Allows(required Role) bool {
	return required.Rank() > 0 && r.Rank() >= required.Rank()
}

// APIKey is one loaded static API key. The plaintext key is never retained:
// only its SHA-256 digest survives parse/load.
type APIKey struct {
	ID     string
	Role   Role
	Digest [sha256.Size]byte
}

// String renders the key identity without the digest. The digest is
// secret-derived material and must never appear in logs or formatted errors.
func (k APIKey) String() string {
	return fmt.Sprintf("APIKey{ID:%q, Role:%q, Digest:<redacted>}", k.ID, k.Role)
}

// Format covers every verb, including %#v, for both APIKey values and pointers.
func (k APIKey) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, k.String())
}

// GoString keeps direct %#v-style callers from falling back to Digest bytes.
func (k APIKey) GoString() string { return k.String() }

// String renders the non-secret API block; only the number of keys is exposed.
func (c APIConfig) String() string {
	return fmt.Sprintf("APIConfig{Addr:%q, ReadHeaderTimeout:%s, ReadTimeout:%s, WriteTimeout:%s, IdleTimeout:%s, ShutdownTimeout:%s, MaxHeaderBytes:%d, MaxJSONBytes:%d, MaxUploadBytes:%d, Keys:%d}",
		c.Addr, c.ReadHeaderTimeout, c.ReadTimeout, c.WriteTimeout, c.IdleTimeout, c.ShutdownTimeout,
		c.MaxHeaderBytes, c.MaxJSONBytes, c.MaxUploadBytes, len(c.Keys))
}

// Format covers every verb, including %#v, for both APIConfig values and
// pointers. Keys are summarized by count, never by digest.
func (c APIConfig) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// GoString keeps direct %#v-style callers from falling back to struct fields.
func (c APIConfig) GoString() string { return c.String() }

// APIConfig is the non-secret HTTP API configuration block. API keys are
// env-only (EnvAPIKeys) and are not part of this YAML block.
type APIConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	MaxJSONBytes      int64         `yaml:"max_json_bytes"`
	MaxUploadBytes    int64         `yaml:"max_upload_bytes"`

	// Keys is populated only from the API_KEYS environment variable. The YAML
	// decoder never fills it, so `api.keys` is a strict-decode error.
	Keys []APIKey `yaml:"-"`
}

// API defaults from spec section 5.1.
const (
	DefaultAPIAddr              = ":8080"
	DefaultAPIReadHeaderTimeout = 5 * time.Second
	DefaultAPIReadTimeout       = 30 * time.Second
	DefaultAPIWriteTimeout      = 60 * time.Second
	DefaultAPIIdleTimeout       = 120 * time.Second
	DefaultAPIShutdownTimeout   = 10 * time.Second
	DefaultAPIMaxHeaderBytes    = 65536
	DefaultAPIMaxJSONBytes      = 1 << 20
	DefaultAPIMaxUploadBytes    = 32 << 20
)

// MinAPIKeyLen is the minimum plaintext key length.
const MinAPIKeyLen = 32

// MaxAPIKeyLen is the maximum plaintext key length.
const MaxAPIKeyLen = 512

// DefaultAPIConfig returns a fresh API configuration with the locked defaults.
func DefaultAPIConfig() APIConfig {
	return APIConfig{
		Addr:              DefaultAPIAddr,
		ReadHeaderTimeout: DefaultAPIReadHeaderTimeout,
		ReadTimeout:       DefaultAPIReadTimeout,
		WriteTimeout:      DefaultAPIWriteTimeout,
		IdleTimeout:       DefaultAPIIdleTimeout,
		ShutdownTimeout:   DefaultAPIShutdownTimeout,
		MaxHeaderBytes:    DefaultAPIMaxHeaderBytes,
		MaxJSONBytes:      DefaultAPIMaxJSONBytes,
		MaxUploadBytes:    DefaultAPIMaxUploadBytes,
		Keys:              nil,
	}
}

// WithDefaults fills zero-valued non-secret fields with the locked defaults.
// Explicit non-zero values are preserved for Validate to check.
func (c APIConfig) WithDefaults() APIConfig {
	d := DefaultAPIConfig()
	if c.Addr == "" {
		c.Addr = d.Addr
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = d.ReadHeaderTimeout
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = d.ReadTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = d.MaxHeaderBytes
	}
	if c.MaxJSONBytes == 0 {
		c.MaxJSONBytes = d.MaxJSONBytes
	}
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = d.MaxUploadBytes
	}
	return c
}

// Validate checks the non-secret API block. It never inspects key material.
func (c APIConfig) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("config: api.addr is required")
	}
	for _, item := range []struct {
		name  string
		value time.Duration
	}{
		{"api.read_header_timeout", c.ReadHeaderTimeout},
		{"api.read_timeout", c.ReadTimeout},
		{"api.write_timeout", c.WriteTimeout},
		{"api.idle_timeout", c.IdleTimeout},
		{"api.shutdown_timeout", c.ShutdownTimeout},
	} {
		if item.value <= 0 {
			return fmt.Errorf("config: %s must be positive", item.name)
		}
	}
	if c.MaxHeaderBytes < 4096 {
		return fmt.Errorf("config: api.max_header_bytes must be at least 4096, got %d", c.MaxHeaderBytes)
	}
	if c.MaxJSONBytes <= 0 {
		return fmt.Errorf("config: api.max_json_bytes must be positive, got %d", c.MaxJSONBytes)
	}
	if c.MaxUploadBytes <= 0 {
		return fmt.Errorf("config: api.max_upload_bytes must be positive, got %d", c.MaxUploadBytes)
	}
	if c.MaxUploadBytes < c.MaxJSONBytes {
		return fmt.Errorf("config: api.max_upload_bytes must be at least api.max_json_bytes")
	}
	return nil
}

// ValidateAPIRuntime requires only the configuration the API process uses. It
// deliberately does not require DATABASE_URL, DATABASE_AGENT_URL,
// OPENCODE_API_KEY, or worker/synth fields.
func (c Config) ValidateAPIRuntime() error {
	if strings.TrimSpace(c.Database.AppURL) == "" {
		return errors.New("config: database.app_url is required (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")
	}
	if err := c.API.Validate(); err != nil {
		return err
	}
	if len(c.API.Keys) == 0 {
		return errors.New("config: API_KEYS must contain at least one key")
	}
	if err := c.Ingest.Validate(); err != nil {
		return err
	}
	if err := c.Match.Validate(); err != nil {
		return err
	}
	return nil
}

// ParseAPIKeys parses the env-only API_KEYS JSON array. The raw key values are
// hashed and discarded; parse errors name only the array index or field, never
// the value.
func ParseAPIKeys(raw string) ([]APIKey, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()

	var entries []struct {
		ID   string `json:"id"`
		Role string `json:"role"`
		Key  string `json:"key"`
	}
	if err := dec.Decode(&entries); err != nil {
		return nil, errors.New("config: API_KEYS must be a JSON array of {id,role,key} objects")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
			Key  string `json:"key"`
		}{}
	}

	keys := make([]APIKey, 0, len(entries))
	ids := make(map[string]struct{}, len(entries))
	digests := make(map[[sha256.Size]byte]struct{}, len(entries))
	for i, entry := range entries {
		id, err := validateAPIKeyID(entry.ID)
		if err != nil {
			return nil, fmt.Errorf("config: API_KEYS[%d]: %w", i, err)
		}
		if _, exists := ids[id]; exists {
			return nil, fmt.Errorf("config: API_KEYS[%d]: duplicate id", i)
		}
		ids[id] = struct{}{}

		role := Role(entry.Role)
		if !role.Valid() {
			return nil, fmt.Errorf("config: API_KEYS[%d]: role must be viewer, approver, or admin", i)
		}
		key, err := validateAPIKeySecret(entry.Key)
		if err != nil {
			return nil, fmt.Errorf("config: API_KEYS[%d]: %w", i, err)
		}
		digest := sha256.Sum256([]byte(key))
		if _, exists := digests[digest]; exists {
			return nil, fmt.Errorf("config: API_KEYS[%d]: duplicate key material", i)
		}
		digests[digest] = struct{}{}
		keys = append(keys, APIKey{ID: id, Role: role, Digest: digest})
	}
	return keys, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("config: API_KEYS must contain exactly one JSON array")
	}
	return nil
}

func validateAPIKeyID(id string) (string, error) {
	if id == "" {
		return "", errors.New("id is required")
	}
	if strings.TrimSpace(id) != id {
		return "", errors.New("id must not have surrounding whitespace")
	}
	if len(id) > 128 {
		return "", errors.New("id must be at most 128 characters")
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return "", errors.New("id must contain only visible ASCII characters")
		}
	}
	return id, nil
}

func validateAPIKeySecret(key string) (string, error) {
	if key == "" {
		return "", errors.New("key is required")
	}
	if strings.TrimSpace(key) != key {
		return "", errors.New("key must not have leading or trailing whitespace")
	}
	if len(key) < MinAPIKeyLen {
		return "", fmt.Errorf("key must be at least %d characters", MinAPIKeyLen)
	}
	if len(key) > MaxAPIKeyLen {
		return "", fmt.Errorf("key must be at most %d characters", MaxAPIKeyLen)
	}
	return key, nil
}

// applyAPIKeysEnv parses API_KEYS into cfg.API.Keys. It is an error-returning
// env binding because malformed key material must abort startup.
func applyAPIKeysEnv(c *Config, raw string) error {
	keys, err := ParseAPIKeys(raw)
	if err != nil {
		return err
	}
	c.API.Keys = keys
	return nil
}
