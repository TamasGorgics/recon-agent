package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	worker := cfg.Worker
	if worker.Concurrency != 4 {
		t.Errorf("worker.concurrency = %d, want 4", worker.Concurrency)
	}
	if worker.PollInterval != time.Second {
		t.Errorf("worker.poll_interval = %s, want 1s", worker.PollInterval)
	}
	if worker.LeaseTTL != 3*time.Minute {
		t.Errorf("worker.lease_ttl = %s, want 3m", worker.LeaseTTL)
	}
	if got, want := worker.DailyBudgetUSD.String(), "5.00"; got != want {
		t.Errorf("worker.daily_budget_usd = %q, want %q", got, want)
	}
	if got, want := worker.DailyBudget().String(), "5"; got != want {
		t.Errorf("worker daily budget = %s, want %s", got, want)
	}
	if worker.MaxAttempts != 3 {
		t.Errorf("worker.max_attempts = %d, want 3", worker.MaxAttempts)
	}
	if worker.ShutdownGrace != 10*time.Second {
		t.Errorf("worker.shutdown_grace = %s, want 10s", worker.ShutdownGrace)
	}
}

func TestWorkerYAMLQuarterDecimal(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	data := `database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
worker:
  concurrency: 2
  poll_interval: 250ms
  lease_ttl: 90s
  daily_budget_usd: "12.50"
  max_attempts: 5
  shutdown_grace: 20s
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Worker.Concurrency != 2 || cfg.Worker.MaxAttempts != 5 {
		t.Fatalf("worker = %+v", cfg.Worker)
	}
	if got, want := cfg.Worker.DailyBudget().String(), "12.5"; got != want {
		t.Fatalf("daily budget = %s, want %s", got, want)
	}
	if cfg.Worker.PollInterval != 250*time.Millisecond || cfg.Worker.LeaseTTL != 90*time.Second {
		t.Fatalf("durations = %s/%s", cfg.Worker.PollInterval, cfg.Worker.LeaseTTL)
	}
}

func TestWorkerYAMLFloatBudgetRejected(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.yaml")
	data := `database:
  url: postgres://recon:recon@localhost:5432/recon?sslmode=disable
worker:
  daily_budget_usd: 12.50
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted a YAML float daily budget")
	}
}

func TestWorkerValidateRanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*WorkerConfig)
		want   string
	}{
		{"concurrency low", func(c *WorkerConfig) { c.Concurrency = 0 }, "worker.concurrency"},
		{"concurrency high", func(c *WorkerConfig) { c.Concurrency = 33 }, "worker.concurrency"},
		{"poll too short", func(c *WorkerConfig) { c.PollInterval = 49 * time.Millisecond }, "worker.poll_interval"},
		{"poll too long", func(c *WorkerConfig) { c.PollInterval = 61 * time.Second }, "worker.poll_interval"},
		{"lease too short", func(c *WorkerConfig) { c.LeaseTTL = 29 * time.Second }, "worker.lease_ttl"},
		{"lease too long", func(c *WorkerConfig) { c.LeaseTTL = 31 * time.Minute }, "worker.lease_ttl"},
		{"budget zero", func(c *WorkerConfig) { c.DailyBudgetUSD = "0" }, "worker.daily_budget_usd"},
		{"budget negative", func(c *WorkerConfig) { c.DailyBudgetUSD = "-1.00" }, "worker.daily_budget_usd"},
		{"budget malformed", func(c *WorkerConfig) { c.DailyBudgetUSD = "five" }, "worker.daily_budget_usd"},
		{"attempts low", func(c *WorkerConfig) { c.MaxAttempts = 0 }, "worker.max_attempts"},
		{"attempts high", func(c *WorkerConfig) { c.MaxAttempts = 11 }, "worker.max_attempts"},
		{"shutdown zero", func(c *WorkerConfig) { c.ShutdownGrace = 0 }, "worker.shutdown_grace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			worker := DefaultWorkerConfig()
			tc.mutate(&worker)
			err := worker.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid worker config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWorkerDefaultValidates(t *testing.T) {
	if err := DefaultWorkerConfig().Validate(); err != nil {
		t.Fatalf("default worker config: %v", err)
	}
}

func TestValidateWorkerRuntimeRequiresRoleDSNs(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.ValidateWorkerRuntime(); err == nil {
		t.Fatal("ValidateWorkerRuntime accepted a config without role DSNs")
	}

	t.Setenv(EnvDatabaseAgentURL, "postgres://agent:secret@db:5432/recon?sslmode=disable")
	cfg, err = LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.ValidateWorkerRuntime(); err == nil || !strings.Contains(err.Error(), EnvDatabaseAppURL) {
		t.Fatalf("ValidateWorkerRuntime error = %v, want it to mention %s", err, EnvDatabaseAppURL)
	}

	t.Setenv(EnvDatabaseAppURL, "postgres://app:secret@db:5432/recon?sslmode=disable")
	cfg, err = LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.ValidateWorkerRuntime(); err != nil {
		t.Fatalf("ValidateWorkerRuntime: %v", err)
	}
}

func TestValidateWorkerRuntimeNeverPrintsSecret(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	t.Setenv(EnvDatabaseAppURL, "postgres://app:topsecret@db:5432/recon?sslmode=disable")
	err = cfg.ValidateWorkerRuntime()
	if err == nil {
		t.Fatal("ValidateWorkerRuntime accepted a config without the agent DSN")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error leaked a DSN secret: %v", err)
	}
}
