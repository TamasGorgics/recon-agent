package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment variable names read by Load.
const (
	EnvConfigFile      = "CONFIG_FILE"
	EnvDatabaseURL     = "DATABASE_URL"
	EnvDatabaseTestURL = "TEST_DATABASE_URL"
	EnvLogLevel        = "LOG_LEVEL"
	EnvAnthropicAPIKey = "ANTHROPIC_API_KEY"
)

type envBinding struct {
	name string
	set  func(*Config, string)
}

// envBindings lists every environment variable that overrides Config. It is
// the source of truth for the .env.example parity test.
var envBindings = []envBinding{
	{EnvDatabaseURL, func(c *Config, v string) { c.Database.URL = v }},
	{EnvDatabaseTestURL, func(c *Config, v string) { c.Database.TestURL = v }},
	{EnvLogLevel, func(c *Config, v string) { c.Log.Level = v }},
	{EnvAnthropicAPIKey, func(c *Config, v string) { c.LLM.AnthropicAPIKey = v }},
}

// DefaultLogLevel is used when neither YAML nor the environment sets a level.
const DefaultLogLevel = "info"

// Config is the complete runtime configuration.
type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	LLM      LLMConfig      `yaml:"llm"`
	Ledger   LedgerConfig   `yaml:"ledger"`
}

// DatabaseConfig holds the Postgres DSNs.
type DatabaseConfig struct {
	URL     string `yaml:"url"`
	TestURL string `yaml:"test_url"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level string `yaml:"level"`
}

// LLMConfig holds model provider credentials. Secrets are optional at M0.
type LLMConfig struct {
	AnthropicAPIKey string `yaml:"anthropic_api_key"`
}

// Load reads configuration from the file named by CONFIG_FILE (when set) and
// then overlays the environment. Precedence is defaults < YAML < environment.
func Load() (Config, error) {
	return LoadFile(os.Getenv(EnvConfigFile))
}

// LoadFile reads configuration from the YAML file at path and then overlays the
// environment. An empty path means no YAML file. A non-empty path that cannot
// be read or parsed is an error; unknown YAML keys are rejected.
func LoadFile(path string) (Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config file %q: %w", path, err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("parse config file %q: %w", path, err)
		}
	}
	applyEnv(&cfg)
	return cfg, nil
}

// Validate checks the configuration required by runtime commands. Secrets stay
// optional at M0.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Database.URL) == "" {
		return errors.New("config: database.url is required (set DATABASE_URL or database.url in CONFIG_FILE)")
	}
	return nil
}

func defaults() Config {
	return Config{
		Log:    LogConfig{Level: DefaultLogLevel},
		Ledger: DefaultLedgerConfig(),
	}
}

func applyEnv(cfg *Config) {
	for _, binding := range envBindings {
		if v := os.Getenv(binding.name); v != "" {
			binding.set(cfg, v)
		}
	}
}
