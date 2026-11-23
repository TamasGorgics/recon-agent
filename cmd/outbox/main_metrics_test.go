package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
)

func outboxMetricsFixture(t *testing.T, addr string) (config.Config, *bytes.Buffer, *slog.Logger, *metrics.Metrics) {
	t.Helper()
	cfg := config.Config{Metrics: config.DefaultMetricsConfig()}
	cfg.Metrics.OutboxAddr = addr
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := metrics.New(metrics.Options{Component: "outbox"})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return cfg, logs, logger, m
}

func TestStartOutboxMetricsServesAndShutsDown(t *testing.T) {
	cfg, logs, logger, m := outboxMetricsFixture(t, "127.0.0.1:0")

	server, err := startOutboxMetrics(context.Background(), cfg, logger, m, false)
	if err != nil {
		t.Fatalf("startOutboxMetrics: %v", err)
	}
	if server == nil {
		t.Fatal("startOutboxMetrics returned nil in long-running mode")
	}
	baseURL := "http://" + server.Addr().String()
	response, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), `recon_build_info{component="outbox"} 1`) {
		t.Fatalf("metrics body is missing the outbox build info:\n%s", body)
	}
	response, err = http.Get(baseURL + "/not-metrics")
	if err != nil {
		t.Fatalf("GET /not-metrics: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", response.StatusCode)
	}
	if !strings.Contains(logs.String(), "metrics listener started") {
		t.Fatalf("startup did not log the metrics listener:\n%s", logs.String())
	}

	started := time.Now()
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s, want <= 2s", elapsed)
	}
}

func TestStartOutboxMetricsOnceDoesNotBind(t *testing.T) {
	// A non-bindable address proves the --once path never touches the port.
	cfg, _, logger, m := outboxMetricsFixture(t, "not-a-listen-address")

	server, err := startOutboxMetrics(context.Background(), cfg, logger, m, true)
	if err != nil {
		t.Fatalf("startOutboxMetrics --once: %v", err)
	}
	if server != nil {
		_ = server.Close()
		t.Fatal("startOutboxMetrics started a listener in --once mode")
	}
}

func TestStartOutboxMetricsBindErrorNamesConfigKey(t *testing.T) {
	cfg, _, logger, m := outboxMetricsFixture(t, "127.0.0.1:0")
	first, err := startOutboxMetrics(context.Background(), cfg, logger, m, false)
	if err != nil {
		t.Fatalf("first startOutboxMetrics: %v", err)
	}
	defer func() { _ = first.Close() }()

	cfg.Metrics.OutboxAddr = first.Addr().String()
	second, err := startOutboxMetrics(context.Background(), cfg, logger, m, false)
	if err == nil {
		_ = second.Close()
		t.Fatal("startOutboxMetrics bound an address already in use")
	}
	if !strings.Contains(err.Error(), "metrics.outbox_addr") {
		t.Fatalf("bind error = %v, want the config key", err)
	}
}
