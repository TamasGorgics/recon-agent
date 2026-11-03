//go:build integration

package matchscore_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/demo"
	"github.com/TamasGorgics/recon-agent/internal/matchscore"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before the gate clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "matchscore: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "matchscore: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestMilestone1Gate is the M1 exit gate. It runs the same demo configuration
// as `make demo-m1` (seed 1, realistic, 14 days), asserts the precision and
// recall thresholds, checks the documented FIN annotation subset, and prints
// the measured values and runtime.
func TestMilestone1Gate(t *testing.T) {
	root := findRepoRoot(t)
	missing, err := matchscore.MissingCoverage(root, matchscore.M1CoverageSubset())
	if err != nil {
		t.Fatalf("annotation scan: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("M1-16 gate: missing // covers: annotations for %s\n%s",
			strings.Join(missing, ", "), matchscore.SubsetRationale)
	}

	db := testdb.New(t)
	started := time.Now()
	summary, err := demo.Run(context.Background(), demo.Options{DBURL: db.DSN})
	if err != nil {
		t.Fatalf("M1 demo pipeline: %v", err)
	}
	elapsed := time.Since(started)

	t.Logf("M1-16 gate: precision=%.4f recall=%.4f seed=%d difficulty=%s days=%d runtime=%s",
		summary.Precision, summary.Recall, summary.Seed, summary.Difficulty, summary.Days,
		elapsed.Round(time.Millisecond))
	t.Logf("M1-16 gate: matches=%d/%d expected_links=%d recalled_links=%d breaks=%d cases=%d",
		summary.CorrectMatches, summary.Matches, summary.ExpectedLinks,
		summary.RecalledLinks, summary.BreakTotal, summary.Cases)
	for _, pass := range summary.PerPass {
		t.Logf("M1-16 gate: pass=%s matches=%d correct=%d precision=%.4f recall=%.4f",
			pass.Pass, pass.Matches, pass.CorrectMatches, pass.Precision, pass.Recall)
	}

	if summary.Precision < 0.99 {
		t.Fatalf("M1-16 gate FAIL: precision %.4f < 0.99 (matches=%d correct=%d)",
			summary.Precision, summary.Matches, summary.CorrectMatches)
	}
	if summary.Recall < 0.95 {
		t.Fatalf("M1-16 gate FAIL: recall %.4f < 0.95 (expected_links=%d recalled=%d)",
			summary.Recall, summary.ExpectedLinks, summary.RecalledLinks)
	}
	if summary.ExpectedLinks == 0 {
		t.Fatal("M1-16 gate FAIL: recall denominator is zero")
	}
}
