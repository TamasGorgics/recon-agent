package metrics

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultGatherTimeout bounds one scrape-time database collector invocation.
// It mirrors the metrics.gather_timeout default; the binaries pass the
// validated config value and unit tests can shorten it.
const DefaultGatherTimeout = 2 * time.Second

// Queryer is the read-only subset of pgx used by the scrape-time collector.
// *pgxpool.Pool and pgx.Tx satisfy it.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// queueQuery is one closed queue gauge query. queue is one of the catalogued
// queue values; the enum never grows at runtime and is never a label built
// from database text.
type queueQuery struct {
	queue string
	sql   string
}

// The three queue queries match the specification predicates exactly. The
// cases and outbox predicates match the existing partial indexes; on a small
// proposals table Postgres may choose a seq scan, with
// proposals_one_active_per_case available when volume makes it worthwhile.
var queueQueries = []queueQuery{
	{
		queue: QueueCasesOpen,
		sql: `SELECT count(*)
		        FROM cases
		       WHERE status = 'open'
		         AND leased_by IS NULL`,
	},
	{
		queue: QueueApprovalsPending,
		sql: `SELECT count(*)
		        FROM proposals
		       WHERE status = 'pending_approval'`,
	},
	{
		queue: QueueOutboxPending,
		sql: `SELECT count(*)
		        FROM outbox
		       WHERE delivered_at IS NULL
		         AND dead = false`,
	},
}

// outboxLagQuery returns one JSON object mapping every topic with pending
// rows to the age of its oldest row, using the database clock. Topics are
// folded into the frozen allow-list (or unknown) in Go, so the database text
// never becomes a label value directly.
const outboxLagQuery = `SELECT COALESCE(
	          jsonb_object_agg(topic, lag), '{}'::jsonb
	        )
	   FROM (
	         SELECT topic,
	                EXTRACT(EPOCH FROM (now() - MIN(created_at)))::double precision AS lag
	           FROM outbox
	          WHERE delivered_at IS NULL
	            AND dead = false
	          GROUP BY topic
	        ) AS pending`

// dbCollector owns the scrape-time database gauges. It wraps the seeded
// queue/lag vectors and the errors counter so one Collect call runs the
// bounded queries, updates the gauges, and emits the errors family after any
// collect failure is counted. Emitting the errors family from this collector
// (rather than a separate registered vector) makes the increment visible in
// the same scrape regardless of gather ordering.
type dbCollector struct {
	queue  *prometheus.GaugeVec
	lag    *prometheus.GaugeVec
	errors *prometheus.CounterVec

	queryer Queryer
	timeout time.Duration
	logger  *slog.Logger
}

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	c.queue.Describe(ch)
	c.lag.Describe(ch)
	c.errors.Describe(ch)
}

// Collect runs at most four bounded aggregate queries. A successful queue
// query sets the fresh count, including zero. On failure the previous gauge
// value is kept (the gauge is not mutated), a bounded metrics/collect error is
// counted, and only a bounded component/queue pair is logged. The collector is
// safe for concurrent gathers: every vector is concurrency-safe and each
// invocation has its own deadline, so a hung database bounds scrape cost at
// GatherTimeout rather than an indeterminate query time.
func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	failures := 0
	for _, query := range queueQueries {
		var depth int64
		if err := c.queryer.QueryRow(ctx, query.sql).Scan(&depth); err != nil {
			failures++
			c.logFailure(query.queue)
			// A shared deadline means the remaining queries cannot succeed;
			// do not spend more time on them.
			if ctx.Err() != nil {
				break
			}
			continue
		}
		c.queue.WithLabelValues(query.queue).Set(float64(depth))
	}

	var raw []byte
	if err := c.queryer.QueryRow(ctx, outboxLagQuery).Scan(&raw); err != nil {
		failures++
		c.logFailure("outbox_lag")
	} else if lags, err := decodeTopicLag(raw); err != nil {
		failures++
		c.logFailure("outbox_lag")
	} else {
		c.publishLag(lags)
	}

	if failures > 0 {
		c.errors.WithLabelValues("metrics", "collect").Add(float64(failures))
	}
	c.queue.Collect(ch)
	c.lag.Collect(ch)
	c.errors.Collect(ch)
}

// publishLag publishes every catalogued topic on a successful query. The
// allow-list is closed and starts at zero so a formerly pending topic does not
// keep a stale age after delivery; unrecognized database topics fold into
// unknown by maximum.
func (c *dbCollector) publishLag(lags map[string]float64) {
	values := make(map[string]float64, len(topicValues))
	for _, topic := range topicValues {
		values[topic] = 0
	}
	for topic, lagSeconds := range lags {
		topic = normalizeTopic(topic)
		if !finite(lagSeconds) || lagSeconds < 0 {
			lagSeconds = 0
		}
		if lagSeconds > values[topic] {
			values[topic] = lagSeconds
		}
	}
	for _, topic := range topicValues {
		c.lag.WithLabelValues(topic).Set(values[topic])
	}
}

func (c *dbCollector) logFailure(queue string) {
	if c.logger == nil {
		return
	}
	// Bounded fields only: never the query text, error text, or a DSN.
	c.logger.Warn("metrics database gauge query failed",
		"component", "metrics", "queue", queue)
}

// decodeTopicLag parses the aggregate JSON object. An unparseable payload is a
// collection error, so the caller keeps the previous gauge values.
func decodeTopicLag(raw []byte) (map[string]float64, error) {
	if len(raw) == 0 {
		return map[string]float64{}, nil
	}
	var lags map[string]float64
	if err := json.Unmarshal(raw, &lags); err != nil {
		return nil, err
	}
	return lags, nil
}
