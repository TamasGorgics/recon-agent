package config

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvMetricsWorkerAddr overrides MetricsConfig.WorkerAddr.
	EnvMetricsWorkerAddr = "METRICS_WORKER_ADDR"
	// EnvMetricsOutboxAddr overrides MetricsConfig.OutboxAddr.
	EnvMetricsOutboxAddr = "METRICS_OUTBOX_ADDR"
)

// Metrics listener defaults from spec section 5.3. Both addresses stay on
// loopback so a developer laptop does not expose an unauthenticated endpoint.
const (
	DefaultMetricsWorkerAddr        = "127.0.0.1:9091"
	DefaultMetricsOutboxAddr        = "127.0.0.1:9092"
	DefaultMetricsReadHeaderTimeout = 5 * time.Second
	DefaultMetricsShutdownTimeout   = 5 * time.Second
	DefaultMetricsGatherTimeout     = 2 * time.Second
)

// MetricsConfig configures the small worker and outbox /metrics listeners.
// The API serves metrics on its existing server and does not use these
// addresses. GatherTimeout is consumed by the scrape-time DB collectors; it is
// validated here so an invalid value fails startup before the listener opens.
type MetricsConfig struct {
	WorkerAddr        string        `yaml:"worker_addr"`
	OutboxAddr        string        `yaml:"outbox_addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	GatherTimeout     time.Duration `yaml:"gather_timeout"`
}

// String renders the non-secret metrics block.
func (c MetricsConfig) String() string {
	return fmt.Sprintf("MetricsConfig{WorkerAddr:%q, OutboxAddr:%q, ReadHeaderTimeout:%s, ShutdownTimeout:%s, GatherTimeout:%s}",
		c.WorkerAddr, c.OutboxAddr, c.ReadHeaderTimeout, c.ShutdownTimeout, c.GatherTimeout)
}

// Format covers every verb, including %#v.
func (c MetricsConfig) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// DefaultMetricsConfig returns the locked loopback defaults.
func DefaultMetricsConfig() MetricsConfig {
	return MetricsConfig{
		WorkerAddr:        DefaultMetricsWorkerAddr,
		OutboxAddr:        DefaultMetricsOutboxAddr,
		ReadHeaderTimeout: DefaultMetricsReadHeaderTimeout,
		ShutdownTimeout:   DefaultMetricsShutdownTimeout,
		GatherTimeout:     DefaultMetricsGatherTimeout,
	}
}

// WithDefaults fills zero-valued fields with the locked defaults. An explicit
// empty address is intentionally preserved for Validate to reject; defaults()
// pre-populates the normal load path, and this helper is for programmatic
// construction.
func (c MetricsConfig) WithDefaults() MetricsConfig {
	d := DefaultMetricsConfig()
	if c.WorkerAddr == "" && c.OutboxAddr == "" && c.ReadHeaderTimeout == 0 &&
		c.ShutdownTimeout == 0 && c.GatherTimeout == 0 {
		return d
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = d.ReadHeaderTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
	if c.GatherTimeout == 0 {
		c.GatherTimeout = d.GatherTimeout
	}
	return c
}

// Validate checks both listen addresses and every duration. A non-loopback
// address is valid: the binaries log a warning because the listener has no
// authentication and network policy is the control (spec section 10).
func (c MetricsConfig) Validate() error {
	for _, item := range []struct {
		name  string
		value string
	}{
		{"metrics.worker_addr", c.WorkerAddr},
		{"metrics.outbox_addr", c.OutboxAddr},
	} {
		if err := validateListenAddr(item.name, item.value); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name  string
		value time.Duration
	}{
		{"metrics.read_header_timeout", c.ReadHeaderTimeout},
		{"metrics.shutdown_timeout", c.ShutdownTimeout},
		{"metrics.gather_timeout", c.GatherTimeout},
	} {
		if item.value <= 0 {
			return fmt.Errorf("config: %s must be positive", item.name)
		}
	}
	return nil
}

// validateListenAddr requires a non-empty host:port with a numeric port.
// Port 0 is accepted so tests and operators can request an ephemeral port.
func validateListenAddr(name, addr string) error {
	if addr == "" {
		return fmt.Errorf("config: %s is required", name)
	}
	if strings.TrimSpace(addr) != addr {
		return fmt.Errorf("config: %s must not have surrounding whitespace", name)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: %s must be a host:port address", name)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("config: %s must use a numeric port between 0 and 65535", name)
	}
	return nil
}
