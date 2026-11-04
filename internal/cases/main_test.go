//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain keeps the migration template current before any case test clones it,
// so the M1-14 migration is applied exactly once per migration version.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "cases: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "cases: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
