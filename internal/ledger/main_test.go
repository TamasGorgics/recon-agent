//go:build integration

package ledger_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain prepares the shared migration template before any ledger schema test
// clones it. The template version check in testdb.EnsureTemplate recreates a
// template left behind by an older migration set.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "ledger: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "ledger: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
