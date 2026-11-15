// covers: FIN-22

package httpguard_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/outbox/httpguard"
)

// fakeBeginner records Begin/BeginTx calls and returns fake transactions. It is
// safe for concurrent use because the tracker concurrency test starts many
// transactions at once.
type fakeBeginner struct {
	mu sync.Mutex

	beginErr    error
	beginTxErr  error
	commitErr   error
	rollbackErr error

	beginCalls   int
	beginTxCalls int
}

func (f *fakeBeginner) Begin(context.Context) (pgx.Tx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginCalls++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f.addTxLocked(), nil
}

func (f *fakeBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginTxCalls++
	if f.beginTxErr != nil {
		return nil, f.beginTxErr
	}
	return f.addTxLocked(), nil
}

func (f *fakeBeginner) addTxLocked() *fakeTx {
	return &fakeTx{commitErr: f.commitErr, rollbackErr: f.rollbackErr}
}

func (f *fakeBeginner) calls() (begin, beginTx int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beginCalls, f.beginTxCalls
}

// fakeTx embeds a nil pgx.Tx and overrides only the lifecycle methods the
// tracker wraps.
type fakeTx struct {
	pgx.Tx

	commitErr   error
	rollbackErr error
}

func (t *fakeTx) Commit(context.Context) error {
	return t.commitErr
}

func (t *fakeTx) Rollback(context.Context) error {
	return t.rollbackErr
}

// recordingTransport counts delegated requests and returns a fixed status.
type recordingTransport struct {
	calls  atomic.Int64
	status int
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{
		StatusCode: r.status,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func mustBegin(t *testing.T, tracker *httpguard.Tracker) pgx.Tx {
	t.Helper()
	tx, err := tracker.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return tx
}

// TestGuardFailsHTTPWhileTransactionOpen is the deliberate FIN-22 violation:
// the same outbound request succeeds outside a transaction and fails with
// ErrHTTPInTransaction while the tracker reports one open.
func TestGuardFailsHTTPWhileTransactionOpen(t *testing.T) {
	ctx := context.Background()
	beginner := &fakeBeginner{}
	tracker := httpguard.NewTracker(beginner)
	base := &recordingTransport{status: http.StatusNoContent}
	guard := &httpguard.Guard{Base: base, InTx: tracker.InTx}
	req := httptest.NewRequest(http.MethodGet, "https://example.invalid/events", nil)

	resp, err := guard.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip outside transaction: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status outside transaction = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := base.calls.Load(); got != 1 {
		t.Fatalf("delegated calls outside transaction = %d, want 1", got)
	}

	tx := mustBegin(t, tracker)
	if !tracker.InTx() {
		t.Fatal("InTx = false after Begin, want true")
	}

	resp, err = guard.RoundTrip(req)
	if resp != nil {
		t.Fatalf("response inside transaction = %+v, want nil", resp)
	}
	if !errors.Is(err, httpguard.ErrHTTPInTransaction) {
		t.Fatalf("RoundTrip inside transaction error = %v, want ErrHTTPInTransaction", err)
	}
	if got := base.calls.Load(); got != 1 {
		t.Fatalf("delegated calls inside transaction = %d, want unchanged 1", got)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after Commit, want false")
	}

	resp, err = guard.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip after Commit: %v", err)
	}
	if got := base.calls.Load(); got != 2 {
		t.Fatalf("delegated calls after Commit = %d, want 2", got)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status after Commit = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

// TestTrackerCommitAndRollbackDecrement covers both Begin and BeginTx, nested
// transaction counting, and idempotent decrementing of the wrapper.
func TestTrackerCommitAndRollbackDecrement(t *testing.T) {
	ctx := context.Background()
	beginner := &fakeBeginner{}
	tracker := httpguard.NewTracker(beginner)

	tx1 := mustBegin(t, tracker)
	tx2, err := tracker.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if !tracker.InTx() {
		t.Fatal("InTx = false with two open transactions, want true")
	}

	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("Commit tx1: %v", err)
	}
	if !tracker.InTx() {
		t.Fatal("InTx = false with one open transaction, want true")
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("Rollback tx2: %v", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after Commit and Rollback, want false")
	}

	if begin, beginTx := beginner.calls(); begin != 1 || beginTx != 1 {
		t.Fatalf("Begin/BeginTx calls = %d/%d, want 1/1", begin, beginTx)
	}

	// Repeating Commit/Rollback on the wrapper must not decrement the count
	// twice; a new transaction still has to be visible.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("repeated Commit: %v", err)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("repeated Rollback: %v", err)
	}
	tx3 := mustBegin(t, tracker)
	if !tracker.InTx() {
		t.Fatal("InTx = false after repeated lifecycle calls and a new Begin, want true")
	}
	if err := tx3.Rollback(ctx); err != nil {
		t.Fatalf("Rollback tx3: %v", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after final Rollback, want false")
	}
}

// TestTrackerDecrementsAfterFailedLifecycle proves a failed Commit or Rollback
// still marks the wrapper closed, matching pgx's close-on-failure behavior.
func TestTrackerDecrementsAfterFailedLifecycle(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	beginner := &fakeBeginner{commitErr: boom, rollbackErr: boom}
	tracker := httpguard.NewTracker(beginner)

	tx := mustBegin(t, tracker)
	if err := tx.Commit(ctx); !errors.Is(err, boom) {
		t.Fatalf("Commit error = %v, want boom", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after failed Commit, want false")
	}

	tx = mustBegin(t, tracker)
	if err := tx.Rollback(ctx); !errors.Is(err, boom) {
		t.Fatalf("Rollback error = %v, want boom", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after failed Rollback, want false")
	}
}

// TestTrackerFailedBeginDoesNotCount proves a Begin/BeginTx error leaves the
// open count untouched.
func TestTrackerFailedBeginDoesNotCount(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("begin failed")
	beginner := &fakeBeginner{beginErr: boom, beginTxErr: boom}
	tracker := httpguard.NewTracker(beginner)

	if _, err := tracker.Begin(ctx); !errors.Is(err, boom) {
		t.Fatalf("Begin error = %v, want boom", err)
	}
	if _, err := tracker.BeginTx(ctx, pgx.TxOptions{}); !errors.Is(err, boom) {
		t.Fatalf("BeginTx error = %v, want boom", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after failed Begin/BeginTx, want false")
	}
}

// TestTrackerConcurrentTransactions starts many tracked transactions at once,
// proves InTx sees them, and proves every Commit decrements exactly once.
func TestTrackerConcurrentTransactions(t *testing.T) {
	ctx := context.Background()
	const count = 32

	beginner := &fakeBeginner{}
	tracker := httpguard.NewTracker(beginner)
	begun := make(chan struct{}, count)
	release := make(chan struct{})
	errs := make(chan error, count)

	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := tracker.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			begun <- struct{}{}
			<-release
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}()
	}

	for i := 0; i < count; i++ {
		<-begun
	}
	if !tracker.InTx() {
		t.Fatal("InTx = false with concurrent open transactions, want true")
	}

	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent transaction: %v", err)
	}
	if tracker.InTx() {
		t.Fatal("InTx = true after all concurrent transactions committed, want false")
	}
}

// TestGuardWithHTTPClient exercises the guard through a real http.Client and
// httptest server: outside succeeds, inside fails, and after rollback succeeds.
func TestGuardWithHTTPClient(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	beginner := &fakeBeginner{}
	tracker := httpguard.NewTracker(beginner)
	client := &http.Client{
		Transport: &httpguard.Guard{Base: http.DefaultTransport, InTx: tracker.InTx},
	}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET outside transaction: %v", err)
	}
	_ = resp.Body.Close()
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits outside transaction = %d, want 1", got)
	}

	tx := mustBegin(t, tracker)
	if _, err := client.Get(server.URL); !errors.Is(err, httpguard.ErrHTTPInTransaction) {
		t.Fatalf("GET inside transaction error = %v, want ErrHTTPInTransaction", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits inside transaction = %d, want unchanged 1", got)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET after Rollback: %v", err)
	}
	_ = resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("server hits after Rollback = %d, want 2", got)
	}
}
