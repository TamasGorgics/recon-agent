package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

const resultsPoolConns int32 = 4

// resultsDSN returns the durable results database DSN. Results are never
// written into the disposable sandbox clone.
func resultsDSN(deps Deps) string {
	if strings.TrimSpace(deps.ResultsDSN) != "" {
		return deps.ResultsDSN
	}
	return strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
}

// sandboxAdminDSN returns the maintenance DSN for clones. TEST_DATABASE_URL is
// preferred when set because integration tests point it at the maintenance
// server; otherwise DATABASE_URL is used.
func sandboxAdminDSN(deps Deps) string {
	if strings.TrimSpace(deps.AdminDSN) != "" {
		return deps.AdminDSN
	}
	if test := strings.TrimSpace(os.Getenv(config.EnvDatabaseTestURL)); test != "" {
		return test
	}
	return strings.TrimSpace(os.Getenv(config.EnvDatabaseURL))
}

func openResultsPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("eval/runner: results database DSN is required (set DATABASE_URL)")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: parse results DSN: %w", err)
	}
	poolConfig.MaxConns = resultsPoolConns
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("eval/runner: open results pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("eval/runner: ping results database: %w", err)
	}
	return pool, nil
}

func newSandboxManager(opts Options, rd *resolvedDataset, deps Deps, logger *slog.Logger) (*sandbox.Manager, error) {
	admin := sandboxAdminDSN(deps)
	if admin == "" {
		return nil, fmt.Errorf("eval/runner: sandbox admin DSN is required (set TEST_DATABASE_URL or DATABASE_URL)")
	}
	baseClock := rd.Config.GeneratedAt
	if baseClock.IsZero() {
		baseClock = dataset.DefaultGeneratedAt
	}
	return sandbox.NewManager(sandbox.Options{
		AdminDSN:  admin,
		Template:  "recon_eval_template",
		MaxConns:  rolePoolSize(opts.Concurrency),
		BaseClock: baseClock,
		Logger:    logger,
	})
}

// rolePoolSize is max(4, concurrency+2) capped at 16.
func rolePoolSize(concurrency int) int32 {
	size := int32(concurrency + 2)
	if size < 4 {
		size = 4
	}
	if size > 16 {
		size = 16
	}
	return size
}

func loadLoadActor() (audit.Actor, error) {
	actor, err := audit.SystemActor("eval")
	if err != nil {
		return audit.Actor{}, fmt.Errorf("eval/runner: build eval actor: %w", err)
	}
	return actor, nil
}

// resolveRuntimeConfig returns the runtime agent/LLM config with the eval
// model/prompt overrides applied. It never reads .env files: config.Load reads
// CONFIG_FILE and the process environment only.
func resolveRuntimeConfig(deps Deps, opts Options) (config.Config, error) {
	var cfg config.Config
	if deps.RuntimeConfig != nil {
		cfg = *deps.RuntimeConfig
	} else {
		loaded, err := config.Load()
		if err != nil {
			return config.Config{}, fmt.Errorf("eval/runner: load configuration: %w", err)
		}
		cfg = loaded
	}
	cfg.LLM.Model = opts.Model
	cfg.Agent.Model = opts.Model
	cfg.Agent.PromptVersion = opts.Prompt
	if (opts.LLM == LLMLive || opts.LLM == LLMRecord) && strings.TrimSpace(cfg.LLM.APIKey) == "" {
		cfg.LLM.APIKey = os.Getenv(config.EnvOpenCodeAPIKey)
	}
	if err := cfg.LLM.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("eval/runner: llm config: %w", err)
	}
	if err := cfg.Agent.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("eval/runner: agent config: %w", err)
	}
	return cfg, nil
}

func moduleRoot(deps Deps) (string, error) {
	if strings.TrimSpace(deps.ModuleRoot) != "" {
		return deps.ModuleRoot, nil
	}
	return FindModuleRoot()
}
