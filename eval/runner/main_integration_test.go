//go:build integration

package runner_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/eval/sandbox"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "runner: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "runner: ensure test template: %v\n", err)
		os.Exit(1)
	}
	manager, err := sandbox.NewManager(sandbox.Options{AdminDSN: adminDSN})
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner: create sandbox manager: %v\n", err)
		os.Exit(1)
	}
	if _, err := manager.EnsureTemplate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "runner: ensure eval template: %v\n", err)
		os.Exit(1)
	}

	prefix := packageDatabasePrefix()
	beforeDatabases, err := listNames(ctx, adminDSN,
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1) ORDER BY datname`, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner: list eval databases before run: %v\n", err)
		os.Exit(1)
	}
	beforeLogins, err := listNames(ctx, adminDSN,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`,
		packageLoginPrefix())
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner: list eval logins before run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	afterDatabases, err := listNames(ctx, adminDSN,
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1) ORDER BY datname`, prefix)
	if err == nil {
		if leftovers := addedNames(beforeDatabases, afterDatabases); len(leftovers) > 0 {
			fmt.Fprintf(os.Stderr, "runner: orphan eval databases after run: %s\n",
				strings.Join(leftovers, ", "))
			if code == 0 {
				code = 1
			}
		}
	}
	afterLogins, err := listNames(ctx, adminDSN,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`,
		packageLoginPrefix())
	if err == nil {
		if leftovers := addedNames(beforeLogins, afterLogins); len(leftovers) > 0 {
			fmt.Fprintf(os.Stderr, "runner: orphan eval logins after run: %s\n",
				strings.Join(leftovers, ", "))
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

func packageDatabasePrefix() string {
	return fmt.Sprintf("recon_eval_%d_", os.Getpid())
}

func packageLoginPrefix() string {
	return fmt.Sprintf("recon_eval_login_%d_", os.Getpid())
}

func listNames(ctx context.Context, adminDSN, query, prefix string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, query, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func addedNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	added := make([]string, 0)
	for _, name := range after {
		if _, ok := seen[name]; !ok {
			added = append(added, name)
		}
	}
	return added
}
