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
	EnvConfigFile       = "CONFIG_FILE"
	EnvDatabaseURL      = "DATABASE_URL"
	EnvDatabaseTestURL  = "TEST_DATABASE_URL"
	EnvDatabaseAppURL   = "DATABASE_APP_URL"
	EnvDatabaseAgentURL = "DATABASE_AGENT_URL"
	EnvLogLevel         = "LOG_LEVEL"
	EnvOpenCodeAPIKey   = "OPENCODE_API_KEY"
	EnvOpenCodeBaseURL  = "OPENCODE_BASE_URL"
	EnvOpenCodeModel    = "OPENCODE_MODEL"
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
	{EnvDatabaseAppURL, func(c *Config, v string) { c.Database.AppURL = v }},
	{EnvDatabaseAgentURL, func(c *Config, v string) { c.Database.AgentURL = v }},
	{EnvLogLevel, func(c *Config, v string) { c.Log.Level = v }},
	{EnvOpenCodeAPIKey, func(c *Config, v string) { c.LLM.APIKey = v }},
	{EnvOpenCodeBaseURL, func(c *Config, v string) { c.LLM.BaseURL = v }},
	{EnvOpenCodeModel, func(c *Config, v string) { c.LLM.Model = v }},
}

// DefaultLogLevel is used when neither YAML nor the environment sets a level.
const DefaultLogLevel = "info"

// Config is the complete runtime configuration.
type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	LLM      LLMConfig      `yaml:"llm"`
	Agent    AgentConfig    `yaml:"agent"`
	Ledger   LedgerConfig   `yaml:"ledger"`
	Ingest   IngestConfig   `yaml:"ingest"`
	Match    MatchConfig    `yaml:"match"`
	Synth    SynthConfig    `yaml:"synth"`
}

// DatabaseConfig holds the Postgres DSNs. AppURL and AgentURL are the
// role-scoped pools (recon_app, recon_agent_ro); they are never superuser DSNs.
type DatabaseConfig struct {
	URL      string `yaml:"url"`
	TestURL  string `yaml:"test_url"`
	AppURL   string `yaml:"app_url"`
	AgentURL string `yaml:"agent_url"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level string `yaml:"level"`
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
	cfg.LLM = normalizeLLMConfig(cfg.LLM)
	cfg.Agent = normalizeAgentConfig(cfg.Agent, cfg.LLM.Model)
	return cfg, nil
}

// Validate checks the configuration required by runtime commands. Secrets stay
// optional at M0.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Database.URL) == "" {
		return errors.New("config: database.url is required (set DATABASE_URL or database.url in CONFIG_FILE)")
	}
	if err := c.Ingest.Validate(); err != nil {
		return err
	}
	if err := c.Match.Validate(); err != nil {
		return err
	}
	if err := c.Synth.Validate(); err != nil {
		return err
	}
	if err := c.LLM.Validate(); err != nil {
		return err
	}
	if err := c.Agent.Validate(); err != nil {
		return err
	}
	return nil
}

func defaults() Config {
	return Config{
		Log:    LogConfig{Level: DefaultLogLevel},
		LLM:    DefaultLLMConfig(),
		Agent:  DefaultAgentConfig(),
		Ledger: DefaultLedgerConfig(),
		Match:  DefaultMatchConfig(),
		Synth:  DefaultSynthConfig(),
	}
}

func applyEnv(cfg *Config) {
	for _, binding := range envBindings {
		if v := os.Getenv(binding.name); v != "" {
			binding.set(cfg, v)
		}
	}
}
