//go:build integration

package audit_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any test clones it.
// testdb.EnsureTemplate drops and recreates a template whose goose version is
// older than the embedded migrations, so adding the M1-05 migration is enough
// to roll the template forward.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "audit: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "audit: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
