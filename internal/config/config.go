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
	EnvAPIKeys          = "API_KEYS"
)

type envBinding struct {
	name string
	set  func(*Config, string) error
}

// envBindings lists every environment variable that overrides Config. It is
// the source of truth for the .env.example parity test.
var envBindings = []envBinding{
	{EnvDatabaseURL, func(c *Config, v string) error { c.Database.URL = v; return nil }},
	{EnvDatabaseTestURL, func(c *Config, v string) error { c.Database.TestURL = v; return nil }},
	{EnvDatabaseAppURL, func(c *Config, v string) error { c.Database.AppURL = v; return nil }},
	{EnvDatabaseAgentURL, func(c *Config, v string) error { c.Database.AgentURL = v; return nil }},
	{EnvLogLevel, func(c *Config, v string) error { c.Log.Level = v; return nil }},
	{EnvOpenCodeAPIKey, func(c *Config, v string) error { c.LLM.APIKey = v; return nil }},
	{EnvOpenCodeBaseURL, func(c *Config, v string) error { c.LLM.BaseURL = v; return nil }},
	{EnvOpenCodeModel, func(c *Config, v string) error { c.LLM.Model = v; return nil }},
	{EnvAPIKeys, applyAPIKeysEnv},
	{EnvPolicyFile, func(c *Config, v string) error { c.Policy.File = v; return nil }},
}

// DefaultLogLevel is used when neither YAML nor the environment sets a level.
const DefaultLogLevel = "info"

// Config is the complete runtime configuration.
type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
	LLM      LLMConfig      `yaml:"llm"`
	Agent    AgentConfig    `yaml:"agent"`
	Worker   WorkerConfig   `yaml:"worker"`
	Ledger   LedgerConfig   `yaml:"ledger"`
	Ingest   IngestConfig   `yaml:"ingest"`
	Match    MatchConfig    `yaml:"match"`
	Synth    SynthConfig    `yaml:"synth"`
	API      APIConfig      `yaml:"api"`
	Policy   PolicyConfig   `yaml:"policy"`
	Verifier VerifierConfig `yaml:"verifier"`
}

// String renders Config without credentials: database DSNs and the LLM API key
// are replaced, and API key digests are summarized by count. This keeps
// accidental %v/%+v logging from exposing secrets.
func (c Config) String() string {
	return fmt.Sprintf("Config{Database:%s, Log:%+v, LLM:%s, Agent:%+v, Worker:%+v, Ledger:%+v, Ingest:%+v, Match:%+v, Synth:%+v, API:%s, Policy:%+v, Verifier:%s}",
		redactedDatabase(c.Database), c.Log, redactedLLM(c.LLM), c.Agent, c.Worker, c.Ledger, c.Ingest, c.Match, c.Synth, c.API, c.Policy, c.Verifier)
}

// Format implements fmt.Formatter so every verb, including %#v, uses the
// redacted representation.
func (c Config) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

func redactedDatabase(d DatabaseConfig) string {
	return fmt.Sprintf("DatabaseConfig{URL:%s, TestURL:%s, AppURL:%s, AgentURL:%s}",
		redactSecret(d.URL), redactSecret(d.TestURL), redactSecret(d.AppURL), redactSecret(d.AgentURL))
}

func redactedLLM(l LLMConfig) string {
	return fmt.Sprintf("LLMConfig{Provider:%q, BaseURL:%q, APIKey:%s, Model:%q, Timeout:%s, MaxAttempts:%d}",
		l.Provider, l.BaseURL, redactSecret(l.APIKey), l.Model, l.Timeout, l.MaxAttempts)
}

func redactSecret(value string) string {
	if value == "" {
		return ""
	}
	return "<redacted>"
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
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	cfg.LLM = normalizeLLMConfig(cfg.LLM)
	cfg.Agent = normalizeAgentConfig(cfg.Agent, cfg.LLM.Model)
	cfg.Worker = normalizeWorkerConfig(cfg.Worker)
	cfg.Verifier = cfg.Verifier.WithDefaults()
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
	if err := c.Verifier.Validate(); err != nil {
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
	if err := c.Worker.Validate(); err != nil {
		return err
	}
	if err := c.API.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Policy.File) == "" {
		return errors.New("config: policy.file is required (set POLICY_FILE or policy.file in CONFIG_FILE)")
	}
	return nil
}

func defaults() Config {
	return Config{
		Log:      LogConfig{Level: DefaultLogLevel},
		LLM:      DefaultLLMConfig(),
		Agent:    DefaultAgentConfig(),
		Worker:   DefaultWorkerConfig(),
		Ledger:   DefaultLedgerConfig(),
		Match:    DefaultMatchConfig(),
		Synth:    DefaultSynthConfig(),
		API:      DefaultAPIConfig(),
		Policy:   PolicyConfig{File: DefaultPolicyFile},
		Verifier: DefaultVerifierConfig(),
	}
}

func applyEnv(cfg *Config) error {
	for _, binding := range envBindings {
		if v := os.Getenv(binding.name); v != "" {
			if err := binding.set(cfg, v); err != nil {
				return err
			}
		}
	}
	return nil
}
