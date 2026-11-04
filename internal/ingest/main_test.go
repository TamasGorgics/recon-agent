//go:build integration

// M1-07 ingestion suite acceptance coverage.
// covers: FIN-40, FIN-41, FIN-42, FIN-50, FIN-51, FIN-55

package ingest_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the shared migration template before any ingest test
// clones it, so the new M1-07 tables exist in every test database.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "ingest: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "ingest: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
