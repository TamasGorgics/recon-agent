// covers: FIN-22

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// stubTx embeds pgx.Tx so validation tests can supply a non-nil transaction.
// Any method call would panic through the nil embedded interface, which is
// exactly what these tests assert does not happen.
type stubTx struct{ pgx.Tx }

// stubBeginner satisfies db.Beginner without touching a database.
type stubBeginner struct{}

func (stubBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("stub: not used")
}

func (stubBeginner) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("stub: not used")
}

func TestEnqueueValidation(t *testing.T) {
	ctx := context.Background()
	validPayload := json.RawMessage(`{"ok":true}`)

	tests := []struct {
		name string
		tx   pgx.Tx
		call func(pgx.Tx) (bool, error)
		want error
	}{
		{
			name: "nil transaction",
			tx:   nil,
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "topic", validPayload, "key")
			},
			want: ErrNoTransaction,
		},
		{
			name: "typed nil transaction",
			tx:   (*stubTx)(nil),
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "topic", validPayload, "key")
			},
			want: ErrNoTransaction,
		},
		{
			name: "empty topic",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "", validPayload, "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "whitespace topic",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "   ", validPayload, "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "empty idempotency key",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "topic", validPayload, "")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "nil payload",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "topic", nil, "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "invalid json",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return Enqueue(ctx, tx, "topic", json.RawMessage(`{"broken":`), "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "empty aggregate type",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return EnqueueAggregate(ctx, tx, "", "id", "topic", validPayload, "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "empty aggregate id",
			tx:   &stubTx{},
			call: func(tx pgx.Tx) (bool, error) {
				return EnqueueAggregate(ctx, tx, "proposal", "", "topic", validPayload, "key")
			},
			want: ErrInvalidMessage,
		},
		{
			name: "aggregate nil transaction",
			tx:   nil,
			call: func(tx pgx.Tx) (bool, error) {
				return EnqueueAggregate(ctx, tx, "proposal", "id", "topic", validPayload, "key")
			},
			want: ErrNoTransaction,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inserted, err := tt.call(tt.tx)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, tt.want)
			}
			if inserted {
				t.Fatal("inserted = true, want false on validation error")
			}
		})
	}
}

func TestNewWorkerDefaultsAndValidation(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))

	worker, err := NewWorker(WorkerOptions{Pool: stubBeginner{}, Clock: clk})
	if err != nil {
		t.Fatalf("NewWorker defaults: %v", err)
	}
	if worker.batchSize != DefaultBatchSize {
		t.Errorf("batch size = %d, want %d", worker.batchSize, DefaultBatchSize)
	}
	if worker.lease != DefaultLease {
		t.Errorf("lease = %s, want %s", worker.lease, DefaultLease)
	}
	if worker.maxAttempts != DefaultMaxAttempts {
		t.Errorf("max attempts = %d, want %d", worker.maxAttempts, DefaultMaxAttempts)
	}
	if worker.baseDelay != DefaultBaseDelay {
		t.Errorf("base delay = %s, want %s", worker.baseDelay, DefaultBaseDelay)
	}
	if worker.maxDelay != DefaultMaxDelay {
		t.Errorf("max delay = %s, want %s", worker.maxDelay, DefaultMaxDelay)
	}
	if worker.pollInterval != DefaultPollInterval {
		t.Errorf("poll interval = %s, want %s", worker.pollInterval, DefaultPollInterval)
	}
	if worker.handlerTimeout != DefaultHandlerTimeout {
		t.Errorf("handler timeout = %s, want %s", worker.handlerTimeout, DefaultHandlerTimeout)
	}
	if worker.metrics == nil || worker.logger == nil || worker.sleep == nil {
		t.Fatal("NewWorker left a nil collaborator")
	}

	valid := WorkerOptions{
		Pool:     stubBeginner{},
		Clock:    clk,
		Handlers: map[string]Handler{"topic": HandlerFunc(func(context.Context, Message) error { return nil })},
	}
	tests := []struct {
		name   string
		mutate func(*WorkerOptions)
	}{
		{name: "nil pool", mutate: func(o *WorkerOptions) { o.Pool = nil }},
		{name: "nil clock", mutate: func(o *WorkerOptions) { o.Clock = nil }},
		{name: "negative batch size", mutate: func(o *WorkerOptions) { o.BatchSize = -1 }},
		{name: "negative lease", mutate: func(o *WorkerOptions) { o.Lease = -time.Second }},
		{name: "negative max attempts", mutate: func(o *WorkerOptions) { o.MaxAttempts = -1 }},
		{name: "negative base delay", mutate: func(o *WorkerOptions) { o.BaseDelay = -time.Second }},
		{name: "negative max delay", mutate: func(o *WorkerOptions) { o.MaxDelay = -time.Second }},
		{name: "negative poll interval", mutate: func(o *WorkerOptions) { o.PollInterval = -time.Second }},
		{name: "negative handler timeout", mutate: func(o *WorkerOptions) { o.HandlerTimeout = -time.Second }},
		{name: "nil handler", mutate: func(o *WorkerOptions) { o.Handlers = map[string]Handler{"topic": nil} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.mutate(&opts)
			if _, err := NewWorker(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("error = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// nilPointerHandler is a Handler whose method is never called; NewWorker must
// reject a typed-nil pointer before it can be stored and served.
type nilPointerHandler struct{}

func (*nilPointerHandler) Handle(context.Context, Message) error { return nil }

// TestNewWorkerRejectsTypedNilDependencies proves NewWorker fails closed when a
// dependency or handler interface holds a typed nil, not just a nil interface.
func TestNewWorkerRejectsTypedNilDependencies(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	validHandler := HandlerFunc(func(context.Context, Message) error { return nil })

	var (
		typedNilPool    *pgxpool.Pool
		typedNilClock   *clock.Fake
		typedNilFunc    HandlerFunc
		typedNilHandler *nilPointerHandler
	)
	tests := []struct {
		name   string
		mutate func(*WorkerOptions)
	}{
		{name: "typed nil pool", mutate: func(o *WorkerOptions) { o.Pool = typedNilPool }},
		{name: "typed nil clock", mutate: func(o *WorkerOptions) { o.Clock = typedNilClock }},
		{name: "typed nil handler func", mutate: func(o *WorkerOptions) {
			o.Handlers = map[string]Handler{"topic": typedNilFunc}
		}},
		{name: "typed nil handler pointer", mutate: func(o *WorkerOptions) {
			o.Handlers = map[string]Handler{"topic": typedNilHandler}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := WorkerOptions{
				Pool:     stubBeginner{},
				Clock:    clk,
				Handlers: map[string]Handler{"topic": validHandler},
			}
			tt.mutate(&opts)
			if _, err := NewWorker(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("error = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestWorkerBackoff(t *testing.T) {
	tests := []struct {
		name      string
		baseDelay time.Duration
		maxDelay  time.Duration
		attempts  int
		want      time.Duration
	}{
		{name: "first failure", baseDelay: time.Second, maxDelay: 5 * time.Second, attempts: 1, want: time.Second},
		{name: "second failure", baseDelay: time.Second, maxDelay: 5 * time.Second, attempts: 2, want: 2 * time.Second},
		{name: "third failure", baseDelay: time.Second, maxDelay: 5 * time.Second, attempts: 3, want: 4 * time.Second},
		{name: "capped", baseDelay: time.Second, maxDelay: 5 * time.Second, attempts: 4, want: 5 * time.Second},
		{name: "far capped", baseDelay: time.Second, maxDelay: 5 * time.Second, attempts: 40, want: 5 * time.Second},
		{name: "odd base capped immediately", baseDelay: 3 * time.Second, maxDelay: 10 * time.Second, attempts: 3, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Worker{baseDelay: tt.baseDelay, maxDelay: tt.maxDelay}
			if got := w.backoff(tt.attempts); got != tt.want {
				t.Fatalf("backoff(%d) = %s, want %s", tt.attempts, got, tt.want)
			}
		})
	}
}
