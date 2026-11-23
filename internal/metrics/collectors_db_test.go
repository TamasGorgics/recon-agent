package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// collectorQueryer is a table-driven fake for the scrape-time gauges. It
// distinguishes the queue queries by their table and returns lag JSON for the
// aggregate query.
type collectorQueryer struct {
	mu       sync.Mutex
	fail     error
	block    bool
	queue    map[string]int64
	lagJSON  string
	queries  []string
	rowCalls int
}

func (q *collectorQueryer) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	q.mu.Lock()
	q.queries = append(q.queries, sql)
	q.rowCalls++
	fail, block := q.fail, q.block
	queue := make(map[string]int64, len(q.queue))
	for table, value := range q.queue {
		queue[table] = value
	}
	lagJSON := q.lagJSON
	q.mu.Unlock()

	return collectorRow{scan: func(dest ...any) error {
		if block {
			<-ctx.Done()
			return ctx.Err()
		}
		if fail != nil {
			return fail
		}
		switch {
		case strings.Contains(sql, "jsonb_object_agg"):
			target, ok := dest[0].(*[]byte)
			if !ok {
				return errors.New("fake: lag target is not *[]byte")
			}
			*target = []byte(lagJSON)
			return nil
		case strings.Contains(sql, "FROM cases"):
			return scanCount(dest, queue[QueueCasesOpen])
		case strings.Contains(sql, "FROM proposals"):
			return scanCount(dest, queue[QueueApprovalsPending])
		case strings.Contains(sql, "FROM outbox"):
			return scanCount(dest, queue[QueueOutboxPending])
		default:
			return errors.New("fake: unexpected query")
		}
	}}
}

func scanCount(dest []any, value int64) error {
	target, ok := dest[0].(*int64)
	if !ok {
		return errors.New("fake: count target is not *int64")
	}
	*target = value
	return nil
}

func (q *collectorQueryer) setFailure(err error) {
	q.mu.Lock()
	q.fail = err
	q.mu.Unlock()
}

func (q *collectorQueryer) callCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.rowCalls
}

type collectorRow struct {
	scan func(dest ...any) error
}

func (r collectorRow) Scan(dest ...any) error { return r.scan(dest...) }

func testCollectorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newDBMetrics(t *testing.T, queryer Queryer, timeout time.Duration) *Metrics {
	t.Helper()
	return mustNew(t, Options{
		Component:     "api",
		Queryer:       queryer,
		GatherTimeout: timeout,
		Logger:        testCollectorLogger(),
	})
}

func queueDepthValues(t *testing.T, views map[string]*familyView) map[string]float64 {
	t.Helper()
	family := mustFamily(t, views, MetricQueueDepth)
	if len(family.series) != len(queueValues) {
		t.Fatalf("queue_depth series = %d, want %d", len(family.series), len(queueValues))
	}
	values := make(map[string]float64, len(family.series))
	for _, series := range family.series {
		queue := labelValue(t, series, "queue")
		values[queue] = gaugeValue(t, &series)
	}
	return values
}

func labelValue(t *testing.T, series seriesView, name string) string {
	t.Helper()
	for _, label := range series.labels {
		if label.name == name {
			return label.value
		}
	}
	t.Fatalf("series %v is missing label %q", series.labels, name)
	return ""
}

func outboxLagValues(t *testing.T, views map[string]*familyView) map[string]float64 {
	t.Helper()
	family := mustFamily(t, views, MetricOutboxLagSeconds)
	values := make(map[string]float64, len(family.series))
	for _, series := range family.series {
		topic := labelValue(t, series, "topic")
		if _, exists := values[topic]; exists {
			t.Fatalf("duplicate topic series %q", topic)
		}
		values[topic] = gaugeValue(t, &series)
	}
	return values
}

func collectErrorCount(t *testing.T, views map[string]*familyView) float64 {
	t.Helper()
	family := mustFamily(t, views, MetricErrorsTotal)
	for _, series := range family.series {
		if labelValue(t, series, "component") == "metrics" &&
			labelValue(t, series, "code") == "collect" {
			return counterValue(t, &series)
		}
	}
	return 0
}

func TestDBCollectorQueueDepth(t *testing.T) {
	queryer := &collectorQueryer{
		queue: map[string]int64{
			QueueCasesOpen:        7,
			QueueApprovalsPending: 0,
			QueueOutboxPending:    5,
		},
		lagJSON: `{}`,
	}
	m := newDBMetrics(t, queryer, time.Second)

	views := gatherViews(t, m.Registry())
	want := map[string]float64{
		QueueCasesOpen:        7,
		QueueApprovalsPending: 0,
		QueueOutboxPending:    5,
	}
	got := queueDepthValues(t, views)
	for queue, value := range want {
		if got[queue] != value {
			t.Errorf("queue_depth{%s} = %v, want %v", queue, got[queue], value)
		}
	}
	if calls := queryer.callCount(); calls != 4 {
		t.Errorf("query calls = %d, want 4 (three queues plus one lag aggregate)", calls)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("registry contract with the DB collector: %v", err)
	}
}

func TestDBCollectorOutboxLagFoldsTopics(t *testing.T) {
	queryer := &collectorQueryer{
		queue: map[string]int64{},
		lagJSON: `{
			"approval.request": 90,
			"approval.update": -5,
			"proposal.applied": 300,
			"report.post": 42
		}`,
	}
	m := newDBMetrics(t, queryer, time.Second)

	got := outboxLagValues(t, gatherViews(t, m.Registry()))
	want := map[string]float64{
		"approval.request": 90,
		"approval.update":  0,
		"approval.cancel":  0,
		"unknown":          300,
	}
	if len(got) != len(want) {
		t.Fatalf("outbox lag series = %v, want exactly %v", got, want)
	}
	for topic, value := range want {
		if got[topic] != value {
			t.Errorf("outbox_lag{%s} = %v, want %v", topic, got[topic], value)
		}
	}
}

func TestDBCollectorKeepLastValueOnQueryError(t *testing.T) {
	queryer := &collectorQueryer{
		queue: map[string]int64{
			QueueCasesOpen:        4,
			QueueApprovalsPending: 2,
			QueueOutboxPending:    6,
		},
		lagJSON: `{"approval.request": 60}`,
	}
	m := newDBMetrics(t, queryer, time.Second)

	before := gatherViews(t, m.Registry())
	beforeQueue := queueDepthValues(t, before)
	beforeLag := outboxLagValues(t, before)

	queryer.setFailure(errors.New("database unavailable"))
	after := gatherViews(t, m.Registry())
	afterQueue := queueDepthValues(t, after)
	afterLag := outboxLagValues(t, after)

	for queue, value := range beforeQueue {
		if afterQueue[queue] != value {
			t.Errorf("queue_depth{%s} after error = %v, want last value %v", queue, afterQueue[queue], value)
		}
	}
	for topic, value := range beforeLag {
		if afterLag[topic] != value {
			t.Errorf("outbox_lag{%s} after error = %v, want last value %v", topic, afterLag[topic], value)
		}
	}
	if got, want := collectErrorCount(t, after), 4.0; got != want {
		t.Errorf("errors{metrics,collect} = %v, want %v (three queue queries plus lag)", got, want)
	}
}

func TestDBCollectorMalformedLagKeepsLastValue(t *testing.T) {
	queryer := &collectorQueryer{
		queue:   map[string]int64{QueueCasesOpen: 1},
		lagJSON: `{"approval.request": 30}`,
	}
	m := newDBMetrics(t, queryer, time.Second)

	before := outboxLagValues(t, gatherViews(t, m.Registry()))
	queryer.mu.Lock()
	queryer.lagJSON = `not-json`
	queryer.mu.Unlock()

	afterViews := gatherViews(t, m.Registry())
	after := outboxLagValues(t, afterViews)
	for topic, value := range before {
		if after[topic] != value {
			t.Errorf("outbox_lag{%s} after malformed payload = %v, want last value %v", topic, after[topic], value)
		}
	}
	if got, want := collectErrorCount(t, afterViews), 1.0; got != want {
		t.Errorf("errors{metrics,collect} = %v, want 1", got)
	}
}

func TestDBCollectorGatherTimeout(t *testing.T) {
	const timeout = 40 * time.Millisecond
	m := newDBMetrics(t, &collectorQueryer{block: true}, timeout)

	start := time.Now()
	views := gatherViews(t, m.Registry())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("blocked gather took %s, want bounded near %s", elapsed, timeout)
	}
	if elapsed < timeout/2 {
		t.Fatalf("blocked gather returned in %s, before the %s timeout", elapsed, timeout)
	}
	if got, want := collectErrorCount(t, views), 2.0; got != want {
		t.Errorf("errors{metrics,collect} = %v, want %v (one queue collector plus lag)", got, want)
	}
	for queue, value := range queueDepthValues(t, views) {
		if value != 0 {
			t.Errorf("queue_depth{%s} after timeout = %v, want seeded 0", queue, value)
		}
	}
	if got := outboxLagValues(t, views)["unknown"]; got != 0 {
		t.Errorf("outbox_lag{unknown} after timeout = %v, want seeded 0", got)
	}
}

func TestDBCollectorDisabledWithoutQueryer(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	views := gatherViews(t, m.Registry())

	for queue, value := range queueDepthValues(t, views) {
		if value != 0 {
			t.Errorf("queue_depth{%s} without queryer = %v, want seeded 0", queue, value)
		}
	}
	lag := outboxLagValues(t, views)
	if len(lag) != 1 || lag["unknown"] != 0 {
		t.Fatalf("outbox lag without queryer = %v, want only the seeded unknown series", lag)
	}
	if got := collectErrorCount(t, views); got != 0 {
		t.Fatalf("errors{metrics,collect} without queryer = %v, want 0", got)
	}
}

func TestDBCollectorLogsBoundedFields(t *testing.T) {
	queryer := &collectorQueryer{fail: errors.New("dial tcp: password=super-secret host=db")}
	logs := &strings.Builder{}
	m := mustNew(t, Options{
		Component:     "api",
		Queryer:       queryer,
		GatherTimeout: time.Second,
		Logger:        slog.New(slog.NewTextHandler(logs, nil)),
	})

	gatherViews(t, m.Registry())

	text := logs.String()
	for _, want := range []string{"component=metrics", "queue=cases_open", "queue=outbox_lag"} {
		if !strings.Contains(text, want) {
			t.Errorf("collector log is missing %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"super-secret", "password=", "postgres://", "SELECT", "FROM outbox"} {
		if strings.Contains(text, leak) {
			t.Errorf("collector log leaked %q:\n%s", leak, text)
		}
	}
}

func TestDBCollectorLabelsStayBounded(t *testing.T) {
	queryer := &collectorQueryer{
		queue:   map[string]int64{QueueCasesOpen: 3},
		lagJSON: `{"case_01JABCDEF0123456789ABCDEF": 10, "approval.request": 2}`,
	}
	m := newDBMetrics(t, queryer, time.Second)
	views := gatherViews(t, m.Registry())

	for _, series := range mustFamily(t, views, MetricQueueDepth).series {
		if len(series.labels) != 1 || series.labels[0].name != "queue" {
			t.Fatalf("queue_depth labels = %v, want only queue", series.labels)
		}
		if !strings.Contains(strings.Join(queueValues, ","), series.labels[0].value) {
			t.Fatalf("queue_depth has non-allow-listed value %q", series.labels[0].value)
		}
	}
	for _, series := range mustFamily(t, views, MetricOutboxLagSeconds).series {
		if len(series.labels) != 1 || series.labels[0].name != "topic" {
			t.Fatalf("outbox_lag labels = %v, want only topic", series.labels)
		}
		topic := series.labels[0].value
		if !strings.Contains(strings.Join(topicValues, ","), topic) {
			t.Fatalf("outbox_lag has non-allow-listed topic %q", topic)
		}
		for _, id := range []string{"case_01JABCDEF0123456789ABCDEF"} {
			if strings.Contains(topic, id) {
				t.Fatalf("outbox_lag leaked an identifier-like topic %q", topic)
			}
		}
	}
}
