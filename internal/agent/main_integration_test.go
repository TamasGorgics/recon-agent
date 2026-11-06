//go:build integration

package agent_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the migration template and asserts this process leaves no
// ephemeral role logins behind.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "agent: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "agent: ensure template: %v\n", err)
		os.Exit(1)
	}

	prefix := testdb.LoginRolePrefix()
	before, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: list login roles before run: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	after, err := listLoginRoles(ctx, adminDSN, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent: list login roles after run: %v\n", err)
		os.Exit(1)
	}
	leftovers := newLoginNames(before, after)
	if len(leftovers) > 0 {
		fmt.Fprintf(os.Stderr, "agent: orphan ephemeral login roles after run: %s\n", strings.Join(leftovers, ", "))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func listLoginRoles(ctx context.Context, adminDSN, prefix string) ([]string, error) {
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

func newLoginNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if _, ok := seen[name]; ok {
			continue
		}
		added = append(added, name)
	}
	return added
}
