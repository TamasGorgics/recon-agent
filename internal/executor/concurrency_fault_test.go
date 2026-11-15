//go:build integration

// covers: FIN-20, FIN-21, FIN-23, FIN-24, FIN-25

package executor_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/invariants"
)

// concurrencyApplyTimeout bounds every concurrent Apply in this file. A stuck
// lock or pool wait fails the test with the context error instead of hanging
// the package.
const concurrencyApplyTimeout = 2 * time.Minute

// TestConcurrencyApplyFiftyGoroutinesExactlyOnce races 50 Apply calls on one
// approved adjustment. SERIALIZABLE plus the proposal row lock and the
// applied_proposals primary key must produce exactly one applied result, one
// ledger transaction keyed proposal:<id>, one applied record, and one
// proposal.applied outbox row. Every other call replays or returns a typed
// not-applicable/stale error; no other outcome is tolerated.
//
// The TestConcurrency prefix is deliberate: `make test-fault` selects the
// fault/invariant/concurrency subset with
// -run 'Fault|Invariants|Check|Concurrency', so the three concurrency tests
// must carry that token.
func TestConcurrencyApplyFiftyGoroutinesExactlyOnce(t *testing.T) {
	const goroutines = 50
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	ctx, cancel := context.WithTimeout(context.Background(), concurrencyApplyTimeout)
	defer cancel()

	start := make(chan struct{})
	results := make([]executor.Result, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.service.Apply(ctx, fixture.proposalID)
		}()
	}
	close(start)
	wg.Wait()

	applied, replayed, tolerated := 0, 0, 0
	var ledgerTxnID string
	for i, err := range errs {
		if err != nil {
			if toleratedRaceError(err) {
				tolerated++
				continue
			}
			t.Fatalf("Apply %d error = %v, want nil, replay, ErrNotApplicable, or a typed stale error", i, err)
		}
		switch result := results[i]; {
		case result.Status == executor.StatusApplied && !result.Replayed:
			applied++
			ledgerTxnID = result.LedgerTxnID
		case result.Status == executor.StatusReplayed && result.Replayed:
			replayed++
		default:
			t.Fatalf("Apply %d result = %+v, want one applied or a replay", i, result)
		}
	}
	if applied != 1 {
		t.Fatalf("applied results = %d, want exactly 1 (replayed=%d tolerated_errors=%d)",
			applied, replayed, tolerated)
	}
	if applied+replayed+tolerated != goroutines {
		t.Fatalf("accounted results = %d, want %d", applied+replayed+tolerated, goroutines)
	}

	w.assertAppliedOnce(t, fixture, ledgerTxnID)
	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = $1`,
		"proposal:"+fixture.proposalID); got != 1 {
		t.Errorf("ledger txns for proposal = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM applied_proposals WHERE proposal_id = $1::uuid`,
		fixture.proposalID); got != 1 {
		t.Errorf("applied_proposals rows = %d, want 1", got)
	}
	if got := w.count(t,
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`,
		"proposal.applied:"+fixture.proposalID); got != 1 {
		t.Errorf("proposal.applied outbox rows = %d, want 1", got)
	}
	if got := w.oneString(t,
		`SELECT status FROM proposals WHERE id = $1::uuid`, fixture.proposalID); got != "applied" {
		t.Errorf("proposal status = %s, want applied", got)
	}

	assertInvariantsOK(t, w)
}

// TestConcurrencyCompetingExecutorsSameAccount applies several approved
// adjustments on different cases and breaks but the same ledger accounts at
// the same time. Every apply must commit, each must own exactly one ledger
// transaction, and the cross-table invariants must still hold.
func TestConcurrencyCompetingExecutorsSameAccount(t *testing.T) {
	// The audit chain head is the one serialization point shared by every
	// apply, and the default five-attempt policy admits at most five
	// simultaneous audit-chain writers. Four contenders exercise the
	// same-account race while staying inside that bound.
	const adjustments = 4
	w := newWorld(t)

	fixtures := make([]*caseFixture, adjustments)
	for i := range fixtures {
		fixtures[i] = w.newCase(t, caseSpec{})
	}

	ctx, cancel := context.WithTimeout(context.Background(), concurrencyApplyTimeout)
	defer cancel()

	start := make(chan struct{})
	results := make([]executor.Result, adjustments)
	errs := make([]error, adjustments)
	var wg sync.WaitGroup
	for i := 0; i < adjustments; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = w.service.Apply(ctx, fixtures[i].proposalID)
		}()
	}
	close(start)
	wg.Wait()

	keys := make([]string, adjustments)
	ledgerTxnIDs := make([]string, adjustments)
	for i, fixture := range fixtures {
		if errs[i] != nil {
			t.Fatalf("Apply %d error = %v, want nil", i, errs[i])
		}
		if results[i].Status != executor.StatusApplied || results[i].Replayed {
			t.Fatalf("Apply %d result = %+v, want a first applied result", i, results[i])
		}
		if results[i].LedgerTxnID == "" {
			t.Fatalf("Apply %d ledger txn id is empty, want the posted adjustment", i)
		}
		ledgerTxnIDs[i] = results[i].LedgerTxnID
		keys[i] = "proposal:" + fixture.proposalID
		w.assertAppliedOnce(t, fixture, results[i].LedgerTxnID)
	}

	if got := w.count(t,
		`SELECT count(*) FROM ledger_txns WHERE idempotency_key = ANY($1::text[])`, keys); got != adjustments {
		t.Errorf("ledger txns for %d proposals = %d, want %d", adjustments, got, adjustments)
	}
	if got := w.count(t,
		`SELECT count(DISTINCT id) FROM ledger_txns WHERE idempotency_key = ANY($1::text[])`, keys); got != adjustments {
		t.Errorf("distinct ledger txn ids = %d, want %d", got, adjustments)
	}
	if got := w.count(t,
		`SELECT count(*) FROM ledger_entries WHERE txn_id IN
		     (SELECT id FROM ledger_txns WHERE idempotency_key = ANY($1::text[]))`, keys); got != 2*adjustments {
		t.Errorf("ledger entries for %d adjustments = %d, want %d", adjustments, got, 2*adjustments)
	}
	if got := w.count(t,
		`SELECT count(DISTINCT ledger_txn_id) FROM applied_proposals WHERE ledger_txn_id = ANY(($1::text[])::uuid[])`,
		ledgerTxnIDs); got != adjustments {
		t.Errorf("distinct applied ledger txns = %d, want %d", got, adjustments)
	}

	assertInvariantsOK(t, w)
}

// failFirstBegin is a db.Beginner wrapper that fails the first BeginTx with a
// serialization error and delegates every later call. It makes the FIN-21
// retry path deterministic without real lock contention.
type failFirstBegin struct {
	delegate db.Beginner
	calls    int
}

func (b *failFirstBegin) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.delegate.Begin(ctx)
}

func (b *failFirstBegin) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	b.calls++
	if b.calls == 1 {
		return nil, &pgconn.PgError{
			Code:    "40001",
			Message: "could not serialize access due to concurrent update",
		}
	}
	return b.delegate.BeginTx(ctx, opts)
}

// TestConcurrencyExecutorSerializationRetry proves db.WithTxPolicy retries a
// serialization failure through executor.Apply: the first BeginTx fails with
// SQLSTATE 40001, the injected RetryPolicy.Sleep records the backoff, and the
// second attempt applies exactly once.
func TestConcurrencyExecutorSerializationRetry(t *testing.T) {
	w := newWorld(t)
	fixture := w.newCase(t, caseSpec{})

	beginner := &failFirstBegin{delegate: w.pool}
	var sleeps []time.Duration

	opts := w.options()
	opts.Pool = beginner
	opts.Retry = db.RetryPolicy{
		MaxAttempts: 2,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}
	svc, err := executor.NewService(opts)
	if err != nil {
		t.Fatalf("new retrying executor service: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), concurrencyApplyTimeout)
	defer cancel()
	result, err := svc.Apply(ctx, fixture.proposalID)
	if err != nil {
		t.Fatalf("Apply after injected 40001 error = %v, want success on retry", err)
	}
	if result.Status != executor.StatusApplied || result.Replayed {
		t.Fatalf("Apply result = %+v, want one first applied result", result)
	}
	if result.LedgerTxnID == "" {
		t.Fatal("Apply ledger txn id is empty, want the posted adjustment")
	}
	if beginner.calls != 2 {
		t.Errorf("BeginTx calls = %d, want 2 (one injected failure, one successful retry)", beginner.calls)
	}
	if len(sleeps) < 1 {
		t.Errorf("RetryPolicy.Sleep calls = %d, want at least 1", len(sleeps))
	}

	w.assertAppliedOnce(t, fixture, result.LedgerTxnID)
	assertInvariantsOK(t, w)
}

// toleratedRaceError reports whether a losing concurrent Apply returned an
// error the exactly-once contract permits: not-applicable or a typed stale
// version. Any other error is a failure.
func toleratedRaceError(err error) bool {
	return errors.Is(err, executor.ErrNotApplicable) || errors.Is(err, state.ErrStale)
}

// assertInvariantsOK runs the read-only cross-table checker on the world's
// maintenance pool and fails with the full problem list.
func assertInvariantsOK(t *testing.T, w *world) {
	t.Helper()
	report, err := invariants.Check(context.Background(), w.db.Pool)
	if err != nil {
		t.Fatalf("invariants.Check error = %v", err)
	}
	if !report.OK() {
		t.Fatalf("invariants.Check problems = %+v, want none", report.Problems)
	}
}
