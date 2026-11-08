//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the migration template and proves this package leaves no
// eval databases or ephemeral login roles behind.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "sandbox: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: ensure test template: %v\n", err)
		os.Exit(1)
	}

	beforeDatabases, err := listDatabases(ctx, adminDSN, databasePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: list eval databases before run: %v\n", err)
		os.Exit(1)
	}
	beforeLogins, err := listLogins(ctx, adminDSN, loginPrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: list eval logins before run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	afterDatabases, err := listDatabases(ctx, adminDSN, databasePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: list eval databases after run: %v\n", err)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	}
	afterLogins, err := listLogins(ctx, adminDSN, loginPrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: list eval logins after run: %v\n", err)
		if code == 0 {
			code = 1
		}
		os.Exit(code)
	}
	leftoverDatabases := newNames(beforeDatabases, afterDatabases)
	leftoverLogins := newNames(beforeLogins, afterLogins)
	if len(leftoverDatabases) > 0 {
		fmt.Fprintf(os.Stderr, "sandbox: orphan eval databases after run: %s\n",
			strings.Join(leftoverDatabases, ", "))
		if code == 0 {
			code = 1
		}
	}
	if len(leftoverLogins) > 0 {
		fmt.Fprintf(os.Stderr, "sandbox: orphan eval logins after run: %s\n",
			strings.Join(leftoverLogins, ", "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func listDatabases(ctx context.Context, adminDSN, prefix string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx,
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1) ORDER BY datname`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func listLogins(ctx context.Context, adminDSN, prefix string) ([]string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx,
		`SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func newNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if _, ok := seen[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	return added
}
