//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any matcher test clones
// it. testdb.EnsureTemplate recreates a template left behind by an older
// migration set, so adding the M1-12 migration is enough to roll it forward.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "match: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "match: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
