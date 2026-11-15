// Package httpguard is a test helper that fails outbound HTTP while a database
// transaction is open (FIN-22).
//
// Tracker wraps a db.Beginner and counts transactions that have begun but not
// yet committed or rolled back. Guard is an http.RoundTripper that refuses to
// delegate to its base transport while the tracker reports an open
// transaction. It is deliberately test-only: the outbox worker never wraps a
// handler in a transaction, and production HTTP transports are not guarded.
package httpguard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/db"
)

// ErrHTTPInTransaction means an outbound HTTP request was attempted while a
// tracked database transaction was open.
var ErrHTTPInTransaction = errors.New("httpguard: outbound HTTP inside a transaction")

// Tracker wraps a db.Beginner and counts open transactions. Begin and BeginTx
// return a wrapper that decrements the count exactly once when the transaction
// is committed or rolled back.
type Tracker struct {
	inner db.Beginner
	open  atomic.Int64
}

var _ db.Beginner = (*Tracker)(nil)

// NewTracker wraps inner. The tracker does not start or commit transactions on
// its own.
func NewTracker(inner db.Beginner) *Tracker {
	return &Tracker{inner: inner}
}

// Begin starts a transaction through the wrapped beginner and tracks it.
func (t *Tracker) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := t.inner.Begin(ctx)
	return t.track(tx, err)
}

// BeginTx starts a transaction through the wrapped beginner and tracks it.
func (t *Tracker) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := t.inner.BeginTx(ctx, opts)
	return t.track(tx, err)
}

// InTx reports whether at least one tracked transaction is open.
func (t *Tracker) InTx() bool {
	return t.open.Load() > 0
}

func (t *Tracker) track(tx pgx.Tx, err error) (pgx.Tx, error) {
	if err != nil {
		return nil, err
	}
	t.open.Add(1)
	return &trackedTx{Tx: tx, tracker: t}, nil
}

func (t *Tracker) done() {
	t.open.Add(-1)
}

// trackedTx embeds the real transaction and decrements the tracker once, even
// if the caller retries Commit/Rollback after an error.
type trackedTx struct {
	pgx.Tx
	tracker *Tracker
	once    sync.Once
}

var _ pgx.Tx = (*trackedTx)(nil)

// Commit commits the underlying transaction and marks it closed.
func (t *trackedTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.once.Do(t.tracker.done)
	return err
}

// Rollback rolls back the underlying transaction and marks it closed.
func (t *trackedTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.once.Do(t.tracker.done)
	return err
}

// Guard is an http.RoundTripper that fails closed while InTx reports an open
// transaction. Outside a transaction it delegates to Base, or to
// http.DefaultTransport when Base is nil.
type Guard struct {
	Base http.RoundTripper
	InTx func() bool
}

var _ http.RoundTripper = (*Guard)(nil)

// RoundTrip rejects the request with ErrHTTPInTransaction while a tracked
// transaction is open and delegates to Base otherwise.
func (g *Guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.InTx != nil && g.InTx() {
		return nil, fmt.Errorf("%w: %s %s", ErrHTTPInTransaction, req.Method, req.URL)
	}
	base := g.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
