//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/outbox"
)

// TestClaimSelectsOnlyDueUndeliveredLiveRows proves dead, delivered, and
// future rows stay untouched while one due row is delivered.
func TestClaimSelectsOnlyDueUndeliveredLiveRows(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)

	dueID := w.insert(t, insertSpec{key: "claim-due"})
	futureID := w.insert(t, insertSpec{key: "claim-future", dueAt: w.clock.Now().Add(time.Minute)})
	past := w.clock.Now().Add(-time.Minute)
	deliveredID := w.insert(t, insertSpec{key: "claim-delivered", dueAt: w.clock.Now().Add(-time.Second), deliveredAt: &past})
	deadID := w.insert(t, insertSpec{key: "claim-dead", dead: true})

	var (
		mu   sync.Mutex
		seen []string
	)
	handler := outbox.HandlerFunc(func(_ context.Context, msg outbox.Message) error {
		mu.Lock()
		seen = append(seen, msg.ID)
		mu.Unlock()
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, nil)

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	if len(seen) != 1 || seen[0] != dueID {
		t.Fatalf("handler saw %v, want [%s]", seen, dueID)
	}
	if row := w.load(t, dueID); row.DeliveredAt == nil {
		t.Error("due row delivered_at = NULL, want set")
	}
	if row := w.load(t, futureID); row.DeliveredAt != nil {
		t.Error("future row delivered_at set, want NULL")
	}
	if row := w.load(t, deliveredID); row.Dead {
		t.Error("delivered row dead = true, want false")
	}
	if row := w.load(t, deadID); row.DeliveredAt != nil {
		t.Error("dead row delivered_at set, want NULL")
	}

	// Nothing else is due yet.
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("second claimed = %d, want 0", claimed)
	}

	// Advancing to the future row's due time claims it.
	w.clock.Advance(time.Minute)
	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("third Process: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("third claimed = %d, want 1", claimed)
	}
	if row := w.load(t, futureID); row.DeliveredAt == nil {
		t.Error("future row delivered_at = NULL after due, want set")
	}
}

// TestClaimOrderAndBatchLimit proves one claim takes the earliest due rows in
// (next_attempt_at, id) order and respects BatchSize.
func TestClaimOrderAndBatchLimit(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	base := w.clock.Now()

	first := w.insert(t, insertSpec{key: "order-1", dueAt: base.Add(-5 * time.Second)})
	second := w.insert(t, insertSpec{key: "order-2", dueAt: base.Add(-4 * time.Second)})
	third := w.insert(t, insertSpec{key: "order-3", dueAt: base.Add(-3 * time.Second)})
	fourth := w.insert(t, insertSpec{key: "order-4", dueAt: base.Add(-2 * time.Second)})
	fifth := w.insert(t, insertSpec{key: "order-5", dueAt: base.Add(-time.Second)})

	var seen []string
	handler := outbox.HandlerFunc(func(_ context.Context, msg outbox.Message) error {
		seen = append(seen, msg.ID)
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.BatchSize = 3
	})

	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if claimed != 3 {
		t.Fatalf("first claimed = %d, want 3", claimed)
	}
	want := []string{first, second, third}
	for i := range want {
		if i >= len(seen) || seen[i] != want[i] {
			t.Fatalf("first batch order = %v, want %v", seen, want)
		}
	}
	for _, id := range []string{fourth, fifth} {
		if row := w.load(t, id); row.DeliveredAt != nil {
			t.Errorf("unbatched row %s delivered_at set, want NULL", id)
		}
	}

	claimed, err = worker.Process(ctx)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("second claimed = %d, want 2", claimed)
	}
	want = []string{first, second, third, fourth, fifth}
	for i := range want {
		if i >= len(seen) || seen[i] != want[i] {
			t.Fatalf("all claims order = %v, want %v", seen, want)
		}
	}
}

// TestClaimLeaseHidesRowAndCommitsBeforeHandler proves the claim transaction
// commits before the handler runs: the handler reads the leased row through a
// different pool, and a concurrent claim sees nothing.
func TestClaimLeaseHidesRowAndCommitsBeforeHandler(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	const lease = 45 * time.Second
	id := w.insert(t, insertSpec{key: "lease-row"})
	claimTime := w.clock.Now()

	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan time.Time, 1)
	handler := outbox.HandlerFunc(func(ctx context.Context, msg outbox.Message) error {
		var leased time.Time
		if err := w.db.Pool.QueryRow(ctx,
			`SELECT next_attempt_at FROM outbox WHERE id = $1::uuid`, msg.ID).Scan(&leased); err != nil {
			return err
		}
		observed <- leased
		close(entered)
		<-release
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.Lease = lease
	})

	firstDone := make(chan struct {
		claimed int
		err     error
	}, 1)
	go func() {
		claimed, err := worker.Process(ctx)
		firstDone <- struct {
			claimed int
			err     error
		}{claimed, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}

	// The lease must already be visible to other connections.
	select {
	case leased := <-observed:
		requireTimeEqual(t, "leased next_attempt_at", leased, claimTime.Add(lease))
	case <-time.After(time.Second):
		t.Fatal("handler did not report the leased timestamp")
	}

	// A second claim while the handler runs sees only the lease, not the row.
	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("concurrent Process: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("concurrent claimed = %d, want 0 while leased", claimed)
	}
	row := w.load(t, id)
	if row.Attempts != 0 {
		t.Fatalf("attempts while handler runs = %d, want 0", row.Attempts)
	}
	if row.DeliveredAt != nil {
		t.Fatal("delivered_at set while handler runs, want NULL")
	}

	close(release)
	select {
	case result := <-firstDone:
		if result.err != nil || result.claimed != 1 {
			t.Fatalf("first Process = %d, %v; want 1, nil", result.claimed, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Process did not finish")
	}
	requireTimeEqual(t, "delivered_at", *w.load(t, id).DeliveredAt, claimTime)
}

// TestClaimSkipsLockedRows proves FOR UPDATE SKIP LOCKED ignores a row another
// transaction currently locks and claims the next due row instead.
func TestClaimSkipsLockedRows(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	lockedID := w.insert(t, insertSpec{key: "skip-locked", dueAt: w.clock.Now().Add(-2 * time.Second)})
	freeID := w.insert(t, insertSpec{key: "skip-free", dueAt: w.clock.Now().Add(-time.Second)})

	lockTx, err := w.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	var locked string
	if err := lockTx.QueryRow(ctx, `SELECT id::text FROM outbox WHERE id = $1::uuid FOR UPDATE`, lockedID).Scan(&locked); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	var seen []string
	handler := outbox.HandlerFunc(func(_ context.Context, msg outbox.Message) error {
		seen = append(seen, msg.ID)
		return nil
	})
	worker := w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
		opts.BatchSize = 2
	})

	type result struct {
		claimed int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		claimed, err := worker.Process(ctx)
		done <- result{claimed, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Process with locked row: %v", got.err)
		}
		if got.claimed != 1 {
			t.Fatalf("claimed = %d, want 1 (locked row skipped)", got.claimed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Process blocked on the locked row; SKIP LOCKED missing")
	}
	if len(seen) != 1 || seen[0] != freeID {
		t.Fatalf("handler saw %v, want [%s]", seen, freeID)
	}
	if row := w.load(t, lockedID); row.DeliveredAt != nil {
		t.Fatal("locked row was delivered while another transaction held its lock")
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("rollback lock transaction: %v", err)
	}
	claimed, err := worker.Process(ctx)
	if err != nil {
		t.Fatalf("Process after unlock: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("after unlock claimed = %d, want 1", claimed)
	}
	if row := w.load(t, lockedID); row.DeliveredAt == nil {
		t.Fatal("locked row delivered_at = NULL after unlock, want set")
	}
}

// TestTwoWorkersSkipLockedKeepClaimsDisjoint proves concurrent workers never
// claim the same due row.
func TestTwoWorkersSkipLockedKeepClaimsDisjoint(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	base := w.clock.Now()
	ids := make(map[string]bool)
	for i := 0; i < 8; i++ {
		id := w.insert(t, insertSpec{
			key:   fmt.Sprintf("concurrent-%d", i),
			dueAt: base.Add(-time.Duration(i+1) * time.Second),
		})
		ids[id] = true
	}

	channel := outbox.NewFakeChannel()
	handler := outbox.HandlerFunc(func(ctx context.Context, msg outbox.Message) error {
		// Widen the overlap window so both claim transactions race.
		time.Sleep(5 * time.Millisecond)
		return channel.Handle(ctx, msg)
	})
	newWorker := func() *outbox.Worker {
		return w.newWorker(t, map[string]outbox.Handler{testTopic: handler}, func(opts *outbox.WorkerOptions) {
			opts.BatchSize = 8
		})
	}

	type result struct {
		claimed int
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		worker := newWorker()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, err := worker.Process(ctx)
			results <- result{claimed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	total := 0
	for res := range results {
		if res.err != nil {
			t.Fatalf("worker Process: %v", res.err)
		}
		total += res.claimed
	}
	if total != len(ids) {
		t.Fatalf("total claimed = %d, want %d", total, len(ids))
	}
	messages := channel.Messages()
	if len(messages) != len(ids) {
		t.Fatalf("unique deliveries = %d, want %d", len(messages), len(ids))
	}
	seen := make(map[string]bool, len(messages))
	for _, msg := range messages {
		if seen[msg.ID] {
			t.Fatalf("message %s delivered by both workers", msg.ID)
		}
		seen[msg.ID] = true
		if !ids[msg.ID] {
			t.Fatalf("unexpected message id %s", msg.ID)
		}
	}
}
