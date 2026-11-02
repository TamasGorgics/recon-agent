package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
	"github.com/TamasGorgics/recon-agent/migrations"
)

const usage = `usage: recon <command> [arguments]

Commands:
  migrate up       apply all pending migrations
  migrate status   print the current schema version and pending migrations
  testdb clean     drop orphan recon_test_% databases

Environment:
  DATABASE_URL       admin DSN used by migrate and testdb clean
  TEST_DATABASE_URL  maintenance DSN used by integration tests (CREATEDB required)
`

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "recon: %v\n", err)
		os.Exit(1)
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
	default:
		return fmt.Errorf("unknown command %q (try: recon help)", args[0])
	}
}

func runMigrate(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: recon migrate <up|status>")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	sqlDB, err := sql.Open("pgx", dsn)
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

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required for testdb clean")
	}
	dropped, err := testdb.CleanupOrphans(ctx, dsn)
	if err != nil {
		return fmt.Errorf("clean test databases: %w", err)
	}
	if dropped == 0 {
		fmt.Println("testdb clean: no orphan databases")
		return nil
	}
	fmt.Printf("testdb clean: dropped %d orphan database(s)\n", dropped)
	return nil
}
