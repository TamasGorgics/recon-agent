package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const workerTelemetryComponent = "recon-agent-worker"

// runWithTelemetry initializes tracing from the standard OTel environment,
// runs work, and flushes after work returns. The flush happens after the poll
// loop / --once path finishes, never before, so normal shutdown does not drop
// spans. A nil or unconfigured endpoint yields a no-op Provider.
//
// opts is a test seam for injecting an in-memory exporter; production callers
// pass none.
func runWithTelemetry(
	ctx context.Context,
	logger *slog.Logger,
	work func(context.Context, *telemetry.Provider) error,
	opts ...telemetry.Option,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	provider, err := telemetry.New(ctx, telemetry.ConfigFromEnv("", ""), opts...)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	if provider.Enabled() {
		logger.Info("telemetry tracing enabled", "component", workerTelemetryComponent)
	}

	runErr := work(ctx, provider)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetry.DefaultShutdownTimeout)
	defer cancel()
	if err := provider.Shutdown(shutdownCtx); err != nil {
		logger.Error("telemetry shutdown failed", "component", workerTelemetryComponent, "error", err)
	}
	return runErr
}
