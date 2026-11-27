//go:build integration

// covers: FIN-52
package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestMain makes the migration template current before any integration test in
// this package clones it.
func TestMain(m *testing.M) {
	ctx := context.Background()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		fmt.Fprintln(os.Stderr, "worker: TEST_DATABASE_URL is required for integration tests")
		os.Exit(1)
	}
	if _, err := testdb.EnsureTemplate(ctx, testdb.Options{AdminDSN: adminDSN}); err != nil {
		fmt.Fprintf(os.Stderr, "worker: ensure template: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestWorkerOnceCreatesNoIntegrityRow proves --once is case-only: it processes
// one case and starts no integrity scheduler, so the database still has zero
// integrity_runs rows after the mode returns.
func TestWorkerOnceCreatesNoIntegrityRow(t *testing.T) {
	d := testdb.New(t)
	once := &fakeOnce{}
	if err := runWorkerMode(context.Background(), true, newFakeLoop(nil, true), once, nil,
		quietWorkerLogger()); err != nil {
		t.Fatalf("runWorkerMode --once: %v", err)
	}
	if once.count() != 1 {
		t.Fatalf("ProcessOnce calls = %d, want 1", once.count())
	}
	var rows int64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM integrity_runs`).Scan(&rows); err != nil {
		t.Fatalf("count integrity_runs: %v", err)
	}
	if rows != 0 {
		t.Fatalf("--once wrote %d integrity_runs rows, want 0", rows)
	}
}
