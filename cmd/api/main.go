// Command api runs the reconciliation HTTP API. It opens only the recon_app
// role pool, serves the nine documented routes, and shuts down gracefully on
// SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/TamasGorgics/recon-agent/internal/api"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	caselifecycle "github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/policy/verify"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateAPIRuntime(); err != nil {
		return err
	}
	logger, err := buildLogger(cfg.Log.Level)
	if err != nil {
		return err
	}

	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return fmt.Errorf("open recon_app pool: %w", err)
	}
	defer app.Close()

	clk := clock.Real{}
	ingestActor, err := audit.SystemActor("ingest")
	if err != nil {
		return err
	}
	ingester, err := ingest.New(ingest.Options{
		DB:      app.Pool,
		Clock:   clk,
		Actor:   ingestActor,
		Sources: cfg.Ingest,
	})
	if err != nil {
		return fmt.Errorf("build ingester: %w", err)
	}
	matchActor, err := audit.SystemActor("matcher")
	if err != nil {
		return err
	}
	calendars, err := bizdays.NewSources(cfg.Ingest)
	if err != nil {
		return fmt.Errorf("build source calendars: %w", err)
	}
	lifecycleService, err := caselifecycle.NewService(caselifecycle.Options{
		DB:                               app.Pool,
		Clock:                            clk,
		Sources:                          calendars,
		MissingSettlementSLABusinessDays: cfg.Cases.MissingSettlementSLABusinessDays,
	})
	if err != nil {
		return fmt.Errorf("build case lifecycle service: %w", err)
	}
	matcher, err := match.New(match.Options{
		DB:        app.Pool,
		Clock:     clk,
		Actor:     matchActor,
		Config:    cfg.Match,
		LateMatch: lifecycleService.ReconcileLateMatchHook,
	})
	if err != nil {
		return fmt.Errorf("build matcher: %w", err)
	}
	caseActor, err := audit.SystemActor("cases")
	if err != nil {
		return err
	}
	caseService, err := cases.NewService(app.Pool, clk, caseActor,
		cases.WithCaseTouchedHook(lifecycleService.OnIngestTouchedCase))
	if err != nil {
		return fmt.Errorf("build case service: %w", err)
	}

	loadedPolicy, err := policy.LoadFile(cfg.Policy.File)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}
	policyEngine, err := policy.NewEngine(loadedPolicy, policy.Options{
		CustomerFundsAccounts: cfg.Ledger.CustomerFundsAccounts,
	})
	if err != nil {
		return fmt.Errorf("build policy engine: %w", err)
	}
	verificationService, err := verify.NewService(app.Pool, verify.OptionsFromConfig(cfg.Verifier))
	if err != nil {
		return fmt.Errorf("build verifier: %w", err)
	}
	limitService, err := limits.NewService(limits.Options{
		Pool:   app.Pool,
		Clock:  clk,
		Config: cfg.AutoApplyLimits,
	})
	if err != nil {
		return fmt.Errorf("build limits service: %w", err)
	}
	stateService, err := state.NewService(app.Pool, clk)
	if err != nil {
		return fmt.Errorf("build proposal state service: %w", err)
	}
	approvalService, err := approval.NewService(approval.Options{
		Pool:             app.Pool,
		Clock:            clk,
		State:            stateService,
		Verifier:         approval.VerifierAdapter{Service: verificationService},
		Limits:           limitService,
		Policy:           approval.Policy{Engine: policyEngine, Loaded: loadedPolicy},
		OnProposalClosed: lifecycleService.OnProposalClosed,
	})
	if err != nil {
		return fmt.Errorf("build approval service: %w", err)
	}

	store := api.NewPostgresStore(app.Pool)
	handler, err := api.New(api.Options{
		Config:     cfg.API,
		Keys:       cfg.API.Keys,
		Logger:     logger,
		Clock:      clk,
		Ingest:     ingester,
		Matcher:    matcher,
		Cases:      caseService,
		CaseReader: store,
		CaseFile:   store,
		Proposals:  store,
		Switches:   api.NewSwitchWriter(app.Pool, clk),
		Decisions:  approvalService,
	})
	if err != nil {
		return fmt.Errorf("build api: %w", err)
	}

	server := &http.Server{
		Addr:              cfg.API.Addr,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: cfg.API.ReadHeaderTimeout,
		ReadTimeout:       cfg.API.ReadTimeout,
		WriteTimeout:      cfg.API.WriteTimeout,
		IdleTimeout:       cfg.API.IdleTimeout,
		MaxHeaderBytes:    cfg.API.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()
	logger.Info("api listening",
		"addr", cfg.API.Addr,
		"viewer_keys", countKeys(cfg.API.Keys, config.RoleViewer),
		"approver_keys", countKeys(cfg.API.Keys, config.RoleApprover),
		"admin_keys", countKeys(cfg.API.Keys, config.RoleAdmin),
	)

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func buildLogger(levelText string) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(levelText)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", levelText)
	}
	return telemetry.NewLogger(os.Stdout, level), nil
}

func countKeys(keys []config.APIKey, role config.Role) int {
	count := 0
	for _, key := range keys {
		if key.Role == role {
			count++
		}
	}
	return count
}
