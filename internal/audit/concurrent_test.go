//go:build integration

// covers: FIN-31
package audit_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

// TestConcurrentAppends runs 100 goroutines, each in its own transaction, and
// asserts a single valid chain: contiguous seq, unique prev_hash links, a clean
// Verify, and no lost writes. This is the R12/FIN-31 concurrency gate.
//
// covers: FIN-31
func TestConcurrentAppends(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := testdb.New(t)
	app := openRolePool(t, d, db.RoleApp)

	const goroutines = 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entry := chainEntry(t, i+1)
			err := db.WithTx(ctx, app, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				_, err := audit.Append(ctx, tx, entry)
				return err
			})
			if err != nil {
				errs <- fmt.Errorf("append %d: %w", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	var count, minSeq, maxSeq, distinctSeq int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), min(seq), max(seq), count(DISTINCT seq) FROM audit_log`).
		Scan(&count, &minSeq, &maxSeq, &distinctSeq); err != nil {
		t.Fatalf("read chain summary: %v", err)
	}
	if count != goroutines || minSeq != 1 || maxSeq != goroutines || distinctSeq != goroutines {
		t.Fatalf("chain summary count=%d min=%d max=%d distinct=%d, want %d/1/%d/%d",
			count, minSeq, maxSeq, distinctSeq, goroutines, goroutines, goroutines)
	}

	var duplicateLinks int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		     SELECT prev_hash FROM audit_log GROUP BY prev_hash HAVING count(*) > 1
		 ) duplicate_links`).Scan(&duplicateLinks); err != nil {
		t.Fatalf("count duplicate prev_hash links: %v", err)
	}
	if duplicateLinks != 0 {
		t.Fatalf("duplicate prev_hash links = %d, want 0", duplicateLinks)
	}

	report, err := audit.Verify(ctx, d.Pool, 0)
	if err != nil {
		t.Fatalf("Verify after concurrent appends: %v", err)
	}
	if !report.OK() {
		t.Fatalf("Verify reported problems after concurrent appends: %+v", report.Problems)
	}
	if report.Checked != goroutines {
		t.Fatalf("Verify checked = %d, want %d", report.Checked, goroutines)
	}
}
