package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const usage = `usage: recon <command> [arguments]

Commands:
  migrate up       apply all pending migrations
  migrate status   print the current schema version and pending migrations
  testdb clean     drop orphan recon_test_% databases and ephemeral login roles
  tieout           independent balance tie-out for a scope and statement date
                   usage: recon tieout --scope <uuid> --date YYYY-MM-DD [--json]
                   exit: 0 tied, 2 unexplained difference, 1 error

Environment:
  CONFIG_FILE        optional YAML config path (defaults < YAML < environment)
  DATABASE_URL       admin DSN used by migrate; fallback for testdb clean
  TEST_DATABASE_URL  maintenance DSN used by integration tests and preferred by testdb clean
`

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "recon: %v\n", err)
		os.Exit(exitCode(err))
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("missing command (try: recon help)")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "migrate":
		return runMigrate(ctx, args[1:])
	case "testdb":
		return runTestDB(ctx, args[1:])
	case "synth":
		return runSynth(ctx, args[1:])
	case "tieout":
		return runTieout(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q (try: recon help)", args[0])
	}
}

func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func runMigrate(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: recon migrate <up|status>")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sqlDB, err := sql.Open("pgx", cfg.Database.URL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("configure goose: %w", err)
	}

	switch args[0] {
	case "up":
		return migrateUp(ctx, sqlDB)
	case "status":
		return migrateStatus(ctx, sqlDB)
	case "down":
		return errors.New("migrate down is not supported: migrations are forward-only")
	default:
		return fmt.Errorf("unknown migrate command %q (usage: recon migrate <up|status>)", args[0])
	}
}

func migrateUp(ctx context.Context, sqlDB *sql.DB) error {
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	version, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	fmt.Printf("migrate up: schema version %d\n", version)
	return nil
}

func migrateStatus(ctx context.Context, sqlDB *sql.DB) error {
	current, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	all, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		return fmt.Errorf("collect migrations: %w", err)
	}
	pending := 0
	for _, m := range all {
		if m.Version > current {
			pending++
		}
	}
	if err := goose.StatusContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migration status: %w", err)
	}
	fmt.Printf("migrate status: current version %d, pending %d\n", current, pending)
	if pending == 0 {
		fmt.Println("migrate status: no pending migrations")
	}
	return nil
}

func runTestDB(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "clean" {
		return errors.New("usage: recon testdb clean")
	}

	dsn, err := testdbCleanDSN()
	if err != nil {
		return err
	}
	dropped, err := testdb.CleanupOrphans(ctx, dsn)
	if err != nil {
		return fmt.Errorf("clean test databases: %w", err)
	}
	roles, err := testdb.CleanupOrphanLogins(ctx, dsn)
	if err != nil {
		return fmt.Errorf("clean ephemeral login roles: %w", err)
	}
	if dropped == 0 && roles == 0 {
		fmt.Println("testdb clean: no orphan databases or ephemeral login roles")
		return nil
	}
	fmt.Printf("testdb clean: dropped %d orphan database(s) and %d ephemeral login role(s)\n", dropped, roles)
	return nil
}

// testdbCleanDSN selects the maintenance DSN for `testdb clean`. Test databases
// live on the TEST_DATABASE_URL server, so that DSN wins; DATABASE_URL is only a
// fallback when TEST_DATABASE_URL is unset (D-3, F-09).
func testdbCleanDSN() (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.Database.TestURL != "" {
		return cfg.Database.TestURL, nil
	}
	if cfg.Database.URL != "" {
		return cfg.Database.URL, nil
	}
	return "", errors.New("config: TEST_DATABASE_URL or DATABASE_URL is required for testdb clean")
}
