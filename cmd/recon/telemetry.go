package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

const reconTelemetryComponent = "recon-agent-cli"

// runWithTelemetry initializes tracing from the standard OTel environment,
// runs work, and flushes after work returns. For `recon agent run` this is
// what makes the scripted demo export its case.run tree to Phoenix. A nil or
// unconfigured endpoint yields a no-op Provider, so other commands are
// unaffected.
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
		logger.Info("telemetry tracing enabled", "component", reconTelemetryComponent)
	}

	runErr := work(ctx, provider)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetry.DefaultShutdownTimeout)
	defer cancel()
	if err := provider.Shutdown(shutdownCtx); err != nil {
		logger.Error("telemetry shutdown failed", "component", reconTelemetryComponent, "error", err)
	}
	return runErr
}
