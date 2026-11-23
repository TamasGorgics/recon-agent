package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsConfigDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := DefaultMetricsConfig()
	if cfg.Metrics != want {
		t.Fatalf("Metrics = %+v, want %+v", cfg.Metrics, want)
	}
	if cfg.Metrics.WorkerAddr != "127.0.0.1:9091" || cfg.Metrics.OutboxAddr != "127.0.0.1:9092" {
		t.Fatalf("metrics defaults left loopback: %+v", cfg.Metrics)
	}
	if err := cfg.Metrics.Validate(); err != nil {
		t.Fatalf("default metrics Validate: %v", err)
	}
}

func TestMetricsConfigEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvMetricsWorkerAddr, "127.0.0.1:19091")
	t.Setenv(EnvMetricsOutboxAddr, "127.0.0.1:19092")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrics.WorkerAddr != "127.0.0.1:19091" || cfg.Metrics.OutboxAddr != "127.0.0.1:19092" {
		t.Fatalf("env did not override metrics addresses: %+v", cfg.Metrics)
	}
}

func TestMetricsConfigYAMLAndPrecedence(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
metrics:
  worker_addr: "127.0.0.1:29091"
  outbox_addr: "127.0.0.1:29092"
  read_header_timeout: 7s
  shutdown_timeout: 8s
  gather_timeout: 3s
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Metrics.WorkerAddr != "127.0.0.1:29091" ||
		cfg.Metrics.OutboxAddr != "127.0.0.1:29092" ||
		cfg.Metrics.ReadHeaderTimeout != 7*time.Second ||
		cfg.Metrics.ShutdownTimeout != 8*time.Second ||
		cfg.Metrics.GatherTimeout != 3*time.Second {
		t.Fatalf("YAML metrics block = %+v", cfg.Metrics)
	}

	t.Setenv(EnvMetricsWorkerAddr, "127.0.0.1:39091")
	cfg, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile with env: %v", err)
	}
	if cfg.Metrics.WorkerAddr != "127.0.0.1:39091" {
		t.Fatalf("env must override YAML, got %q", cfg.Metrics.WorkerAddr)
	}
	if cfg.Metrics.OutboxAddr != "127.0.0.1:29092" {
		t.Fatalf("unrelated YAML address = %q", cfg.Metrics.OutboxAddr)
	}
}

func TestMetricsConfigStrictDecodeRejectsUnknownKeys(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
metrics:
  worker_addr: "127.0.0.1:9091"
  unknown_key: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatalf("LoadFile unknown metrics key = %v, want strict-decode error", err)
	}
}

func TestMetricsConfigValidate(t *testing.T) {
	valid := DefaultMetricsConfig()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default Validate: %v", err)
	}
	nonLoopback := valid
	nonLoopback.WorkerAddr = "0.0.0.0:9091"
	nonLoopback.OutboxAddr = "[::]:9092"
	if err := nonLoopback.Validate(); err != nil {
		t.Fatalf("non-loopback address must validate (warning is at startup): %v", err)
	}
	ephemeral := valid
	ephemeral.WorkerAddr = "127.0.0.1:0"
	ephemeral.OutboxAddr = "[::1]:0"
	if err := ephemeral.Validate(); err != nil {
		t.Fatalf("ephemeral port must validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*MetricsConfig)
		want   string
	}{
		{"empty worker", func(c *MetricsConfig) { c.WorkerAddr = "" }, "metrics.worker_addr is required"},
		{"missing port", func(c *MetricsConfig) { c.WorkerAddr = "127.0.0.1" }, "host:port"},
		{"non-numeric port", func(c *MetricsConfig) { c.OutboxAddr = "127.0.0.1:http" }, "numeric port"},
		{"port too large", func(c *MetricsConfig) { c.OutboxAddr = "127.0.0.1:70000" }, "numeric port"},
		{"surrounding whitespace", func(c *MetricsConfig) { c.WorkerAddr = " 127.0.0.1:9091" }, "surrounding whitespace"},
		{"zero header timeout", func(c *MetricsConfig) { c.ReadHeaderTimeout = 0 }, "read_header_timeout"},
		{"negative shutdown timeout", func(c *MetricsConfig) { c.ShutdownTimeout = -time.Second }, "shutdown_timeout"},
		{"zero gather timeout", func(c *MetricsConfig) { c.GatherTimeout = 0 }, "gather_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultMetricsConfig()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want %q", err, test.want)
			}
		})
	}
}

func TestConfigValidateIncludesMetrics(t *testing.T) {
	clearEnv(t)
	cfg := defaults()
	cfg.Database.URL = "postgres://recon:recon@localhost:5432/recon?sslmode=disable"
	cfg.Metrics.GatherTimeout = 0

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "metrics.gather_timeout") {
		t.Fatalf("Config.Validate = %v, want metrics.gather_timeout", err)
	}
}
