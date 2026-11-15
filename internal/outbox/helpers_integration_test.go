//go:build integration

// covers: FIN-22

package outbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

const (
	testTopic      = "approval.request"
	testOtherTopic = "report.post"
)

var outboxSeq atomic.Int64

// world is one isolated test database with a real recon_app pool and a fake
// worker clock one minute ahead of the database clock so rows written by
// Enqueue (CURRENT_TIMESTAMP) are immediately due.
type world struct {
	db       *testdb.DB
	pool     *pgxpool.Pool
	clock    *clock.Fake
	baseTime time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	d := testdb.NewWithOptions(t, testdb.Options{MaxConns: 8})

	var dbNow time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT CURRENT_TIMESTAMP`).Scan(&dbNow); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	dbNow = dbNow.UTC().Truncate(time.Microsecond)

	rolePool, err := db.OpenRole(ctx, db.RoleApp, d.DSNFor(db.RoleApp))
	if err != nil {
		t.Fatalf("open recon_app pool: %v", err)
	}
	t.Cleanup(rolePool.Close)

	return &world{
		db:       d,
		pool:     rolePool.Pool,
		clock:    clock.NewFake(dbNow.Add(time.Minute)),
		baseTime: dbNow,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// insertSpec describes one outbox row inserted directly for deterministic
// due/attempt/lease tests.
type insertSpec struct {
	topic       string
	payload     string
	key         string
	attempts    int
	dueAt       time.Time
	deliveredAt *time.Time
	dead        bool
}

// insert writes one row through the maintenance pool and returns its id.
func (w *world) insert(t *testing.T, spec insertSpec) string {
	t.Helper()
	if spec.topic == "" {
		spec.topic = testTopic
	}
	if spec.payload == "" {
		spec.payload = `{"seeded":true}`
	}
	if spec.key == "" {
		spec.key = fmt.Sprintf("%s-%d", t.Name(), outboxSeq.Add(1))
	}
	if spec.dueAt.IsZero() {
		spec.dueAt = w.clock.Now().Add(-time.Second)
	}

	var delivered any
	if spec.deliveredAt != nil {
		delivered = *spec.deliveredAt
	}

	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at, attempts, delivered_at, dead)
		 VALUES ('outbox', $1, $2, $3::jsonb, $1, $4, $5, $6, $7, $8)
		 RETURNING id::text`,
		spec.key, spec.topic, spec.payload, w.baseTime, spec.dueAt,
		spec.attempts, delivered, spec.dead).Scan(&id); err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
	return id
}

type outboxRow struct {
	ID             string
	AggregateType  string
	AggregateID    string
	Topic          string
	Payload        []byte
	IdempotencyKey string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
	Attempts       int
	NextAttemptAt  time.Time
	Dead           bool
}

func (w *world) load(t *testing.T, id string) outboxRow {
	t.Helper()
	var row outboxRow
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT id::text, aggregate_type, aggregate_id, topic, payload,
		        idempotency_key, created_at, delivered_at, attempts,
		        next_attempt_at, dead
		   FROM outbox
		  WHERE id = $1::uuid`, id).
		Scan(&row.ID, &row.AggregateType, &row.AggregateID, &row.Topic,
			&row.Payload, &row.IdempotencyKey, &row.CreatedAt, &row.DeliveredAt,
			&row.Attempts, &row.NextAttemptAt, &row.Dead); err != nil {
		t.Fatalf("load outbox row %s: %v", id, err)
	}
	return row
}

func (w *world) loadByKey(t *testing.T, key string) outboxRow {
	t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT id::text FROM outbox WHERE idempotency_key = $1`, key).Scan(&id); err != nil {
		t.Fatalf("find outbox row by key %q: %v", key, err)
	}
	return w.load(t, id)
}

func (w *world) countByKey(t *testing.T, key string) int {
	t.Helper()
	var count int
	if err := w.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE idempotency_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count outbox rows by key %q: %v", key, err)
	}
	return count
}

// newWorker builds a worker with test-friendly defaults. mutate may override
// any option, including BatchSize, Metrics, and Sleep.
func (w *world) newWorker(t *testing.T, handlers map[string]outbox.Handler, mutate func(*outbox.WorkerOptions)) *outbox.Worker {
	t.Helper()
	opts := outbox.WorkerOptions{
		Pool:           w.pool,
		Handlers:       handlers,
		Clock:          w.clock,
		BatchSize:      10,
		Lease:          30 * time.Second,
		MaxAttempts:    5,
		BaseDelay:      time.Second,
		MaxDelay:       time.Minute,
		PollInterval:   time.Millisecond,
		HandlerTimeout: 5 * time.Second,
		Logger:         discardLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	worker, err := outbox.NewWorker(opts)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return worker
}

func openExecutorPool(t *testing.T, w *world) *pgxpool.Pool {
	t.Helper()
	rolePool, err := db.OpenRole(context.Background(), db.RoleExecutor, w.db.DSNFor(db.RoleExecutor))
	if err != nil {
		t.Fatalf("open recon_executor pool: %v", err)
	}
	t.Cleanup(rolePool.Close)
	return rolePool.Pool
}

func requireTimeEqual(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %s, want %s", what, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func requireJSONEqual(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s: decode got JSON: %v", what, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: decode want JSON: %v", what, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %s, want %s", what, got, want)
	}
}

// metricsRecorder is a concurrency-safe outbox.Metrics implementation.
type metricsRecorder struct {
	mu        sync.Mutex
	delivered map[string]int
	failed    map[string]int
	dead      map[string][]int
}

func newMetricsRecorder() *metricsRecorder {
	return &metricsRecorder{
		delivered: make(map[string]int),
		failed:    make(map[string]int),
		dead:      make(map[string][]int),
	}
}

func (m *metricsRecorder) Delivered(topic string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered[topic]++
}

func (m *metricsRecorder) Failed(topic string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed[topic]++
}

func (m *metricsRecorder) DeadLettered(topic string, attempts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dead[topic] = append(m.dead[topic], attempts)
}

func (m *metricsRecorder) deliveredCount(topic string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delivered[topic]
}

func (m *metricsRecorder) failedCount(topic string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed[topic]
}

func (m *metricsRecorder) deadAttempts(topic string) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.dead[topic]...)
}

// logBuffer is a concurrency-safe slog writer for assertions on log text.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
