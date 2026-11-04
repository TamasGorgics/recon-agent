//go:build integration

package tieout_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the shared migration template before any tie-out test
// clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "tieout: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "tieout: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
