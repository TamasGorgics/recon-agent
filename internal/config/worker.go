package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Worker defaults from the M2-10 locked contract (spec section 9).
const (
	DefaultWorkerConcurrency    = 4
	DefaultWorkerPollInterval   = time.Second
	DefaultWorkerLeaseTTL       = 3 * time.Minute
	DefaultWorkerDailyBudgetUSD = "5.00"
	DefaultWorkerMaxAttempts    = 3
	DefaultWorkerShutdownGrace  = 10 * time.Second
)

// WorkerConfig configures the background case worker. DailyBudgetUSD is a
// decimal string so a YAML float is a configuration error rather than silently
// rounded money.
type WorkerConfig struct {
	Concurrency    int           `yaml:"concurrency"`
	PollInterval   time.Duration `yaml:"poll_interval"`
	LeaseTTL       time.Duration `yaml:"lease_ttl"`
	DailyBudgetUSD DecimalString `yaml:"daily_budget_usd"`
	MaxAttempts    int           `yaml:"max_attempts"`
	ShutdownGrace  time.Duration `yaml:"shutdown_grace"`
}

// DefaultWorkerConfig returns the locked M2-10 defaults.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Concurrency:    DefaultWorkerConcurrency,
		PollInterval:   DefaultWorkerPollInterval,
		LeaseTTL:       DefaultWorkerLeaseTTL,
		DailyBudgetUSD: DefaultWorkerDailyBudgetUSD,
		MaxAttempts:    DefaultWorkerMaxAttempts,
		ShutdownGrace:  DefaultWorkerShutdownGrace,
	}
}

// DailyBudget returns the parsed daily spend cap. Callers must run Validate
// first; an invalid value returns zero.
func (c WorkerConfig) DailyBudget() decimal.Decimal {
	value, err := decimal.NewFromString(strings.TrimSpace(c.DailyBudgetUSD.String()))
	if err != nil {
		return decimal.Zero
	}
	return value
}

// Validate checks the M2-10 worker block ranges.
func (c WorkerConfig) Validate() error {
	if c.Concurrency < 1 || c.Concurrency > 32 {
		return fmt.Errorf("config: worker.concurrency must be between 1 and 32, got %d", c.Concurrency)
	}
	if c.PollInterval < 50*time.Millisecond || c.PollInterval > time.Minute {
		return fmt.Errorf("config: worker.poll_interval must be between 50ms and 1m, got %s", c.PollInterval)
	}
	if c.LeaseTTL < 30*time.Second || c.LeaseTTL > 30*time.Minute {
		return fmt.Errorf("config: worker.lease_ttl must be between 30s and 30m, got %s", c.LeaseTTL)
	}
	budget, err := decimal.NewFromString(strings.TrimSpace(c.DailyBudgetUSD.String()))
	if err != nil {
		return errors.New("config: worker.daily_budget_usd must be a decimal string")
	}
	if !budget.IsPositive() {
		return fmt.Errorf("config: worker.daily_budget_usd must be positive, got %q", c.DailyBudgetUSD)
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return fmt.Errorf("config: worker.max_attempts must be between 1 and 10, got %d", c.MaxAttempts)
	}
	if c.ShutdownGrace <= 0 {
		return fmt.Errorf("config: worker.shutdown_grace must be positive, got %s", c.ShutdownGrace)
	}
	return nil
}

// normalizeWorkerConfig trims the decimal string so spacing from YAML or the
// environment never reaches the parser.
func normalizeWorkerConfig(worker WorkerConfig) WorkerConfig {
	worker.DailyBudgetUSD = DecimalString(strings.TrimSpace(worker.DailyBudgetUSD.String()))
	return worker
}
