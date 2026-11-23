package main

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/metrics"
)

// TestBuildWorkerWiresOutboxMetrics proves the command passes the process
// registry's outbox adapter into the worker options instead of the no-op
// default, and that a nil registry still constructs a worker.
func TestBuildWorkerWiresOutboxMetrics(t *testing.T) {
	clearCmdEnv(t)
	setOutboxEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	processMetrics, err := metrics.New(metrics.Options{Component: "outbox"})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	runtime, err := buildWorker(buildOptions{
		Config:  cfg,
		Pool:    &fakePool{},
		Metrics: processMetrics,
	})
	if err != nil {
		t.Fatalf("buildWorker: %v", err)
	}
	if _, ok := runtime.options.Metrics.(*metrics.OutboxMetrics); !ok {
		t.Fatalf("worker metrics = %T, want *metrics.OutboxMetrics", runtime.options.Metrics)
	}

	withoutMetrics, err := buildWorker(buildOptions{Config: cfg, Pool: &fakePool{}})
	if err != nil {
		t.Fatalf("buildWorker without metrics: %v", err)
	}
	if withoutMetrics.options.Metrics != nil {
		t.Fatalf("worker metrics without a registry = %T, want nil (no-op)", withoutMetrics.options.Metrics)
	}
}
