package metrics

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Options configures one process metrics registry.
type Options struct {
	// Component is the build_info component for this process. It must be one
	// of api, worker, or outbox.
	Component string
	// GoCollector registers client_golang's Go runtime collector. Long-running
	// binaries enable it; unit tests usually leave it off for deterministic
	// business-family output.
	GoCollector bool
	// ProcessCollector registers client_golang's process collector. It is
	// exempt from the business-cardinality guard.
	ProcessCollector bool
	// Queryer enables the scrape-time database gauges (queue depth and outbox
	// lag) for this process's recon_app pool. Nil keeps the static seeded
	// gauges and issues no database query.
	Queryer Queryer
	// GatherTimeout bounds each scrape-time database collector invocation. It
	// is the metrics.gather_timeout config value; values <= 0 use
	// DefaultGatherTimeout.
	GatherTimeout time.Duration
	// Logger receives bounded collector failure logs (component/queue only,
	// never SQL, error text, or a DSN). Nil disables collector logging.
	Logger *slog.Logger
}

// Metrics owns one isolated Prometheus registry, its typed catalog
// collectors, and the helpers that instrument them. Counter values are
// process-local.
type Metrics struct {
	reg       *prometheus.Registry
	component string

	buildInfo     *prometheus.GaugeVec
	matchRuns     *prometheus.CounterVec
	matches       *prometheus.CounterVec
	breaks        *prometheus.CounterVec
	caseAge       *prometheus.HistogramVec
	queueDepth    *prometheus.GaugeVec
	agentRuns     *prometheus.CounterVec
	agentCost     *prometheus.CounterVec
	approvalDelay *prometheus.HistogramVec
	errors        *prometheus.CounterVec
	outboxLag     *prometheus.GaugeVec
	deadLetters   *prometheus.CounterVec
	capUsage      *prometheus.GaugeVec
	tieout        *prometheus.GaugeVec
	integrity     *prometheus.GaugeVec
}

// New validates the frozen catalog and returns a registry with every business
// family registered and seeded. It never uses the default global registry.
func New(opts Options) (*Metrics, error) {
	if !slices.Contains(buildComponentValues, opts.Component) {
		return nil, fmt.Errorf("metrics: component %q must be one of api, worker, outbox", opts.Component)
	}
	if err := validateCatalog(catalog); err != nil {
		return nil, err
	}

	m := &Metrics{
		reg:       prometheus.NewRegistry(),
		component: opts.Component,
	}
	m.buildInfo = newGaugeVec(MetricBuildInfo)
	m.matchRuns = newCounterVec(MetricMatchRunsTotal)
	m.matches = newCounterVec(MetricMatchesTotal)
	m.breaks = newCounterVec(MetricBreaksTotal)
	m.caseAge = newHistogramVec(MetricCaseAgeSeconds)
	m.queueDepth = newGaugeVec(MetricQueueDepth)
	m.agentRuns = newCounterVec(MetricAgentRunsTotal)
	m.agentCost = newCounterVec(MetricAgentCostUSDTotal)
	m.approvalDelay = newHistogramVec(MetricApprovalLatencySeconds)
	m.errors = newCounterVec(MetricErrorsTotal)
	m.outboxLag = newGaugeVec(MetricOutboxLagSeconds)
	m.deadLetters = newCounterVec(MetricOutboxDeadLettersTotal)
	m.capUsage = newGaugeVec(MetricAutoApplyCapUsageRatio)
	m.tieout = newGaugeVec(MetricTieoutUnexplainedMinor)
	m.integrity = newGaugeVec(MetricIntegrityJobStatus)

	business := []prometheus.Collector{
		m.buildInfo, m.matchRuns, m.matches, m.breaks, m.caseAge,
		m.agentRuns, m.agentCost, m.approvalDelay,
		m.deadLetters, m.capUsage, m.tieout, m.integrity,
	}
	// With a queryer, the queue, outbox-lag, and errors families are owned by
	// one scrape-time collector that wraps the same seeded vectors.
	// Registering both a raw vector and the wrapper would duplicate the
	// family; emitting the errors family from the collector also makes a
	// collect failure visible in the same scrape.
	if opts.Queryer != nil {
		gatherTimeout := opts.GatherTimeout
		if gatherTimeout <= 0 {
			gatherTimeout = DefaultGatherTimeout
		}
		business = append(business, &dbCollector{
			queue:   m.queueDepth,
			lag:     m.outboxLag,
			errors:  m.errors,
			queryer: opts.Queryer,
			timeout: gatherTimeout,
			logger:  opts.Logger,
		})
	} else {
		business = append(business, m.queueDepth, m.errors, m.outboxLag)
	}
	for _, collector := range business {
		if err := m.reg.Register(collector); err != nil {
			return nil, fmt.Errorf("metrics: registering catalog collector: %w", err)
		}
	}

	if opts.GoCollector {
		if err := m.reg.Register(collectors.NewGoCollector()); err != nil {
			return nil, fmt.Errorf("metrics: registering Go collector: %w", err)
		}
	}
	if opts.ProcessCollector {
		if err := m.reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
			return nil, fmt.Errorf("metrics: registering process collector: %w", err)
		}
	}

	m.seed()
	return m, nil
}

// Registry returns the isolated registry. Callers hand it to promhttp; they
// must not register it on the default global registry.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Component returns the build_info component this process registered.
func (m *Metrics) Component() string { return m.component }

// Validate gathers the registry and enforces the catalog and cardinality
// contract. It is the executable version of the metric contract.
func (m *Metrics) Validate() error { return ValidateRegistry(m.reg) }

// ObserveMatchRun records one committed matcher run, its accepted matches per
// rule, and its detected break sides. Invalid or ID-like values fold to
// unknown; non-positive map counts are ignored.
func (m *Metrics) ObserveMatchRun(source string, matchesByRule, breaksBySide map[string]int) {
	source = normalizeSource(source)
	m.matchRuns.WithLabelValues(source).Inc()
	for rule, count := range matchesByRule {
		if count <= 0 {
			continue
		}
		m.matches.WithLabelValues(source, normalizeRule(rule)).Add(float64(count))
	}
	for side, count := range breaksBySide {
		if count <= 0 {
			continue
		}
		m.breaks.WithLabelValues(source, normalizeSide(side)).Add(float64(count))
	}
}

// ObserveCaseAge records one case status observation. Negative ages from clock
// skew clamp to zero; non-finite ages are ignored.
func (m *Metrics) ObserveCaseAge(status string, ageSeconds float64) {
	if !finite(ageSeconds) {
		return
	}
	if ageSeconds < 0 {
		ageSeconds = 0
	}
	m.caseAge.WithLabelValues(normalizeStatus(status)).Observe(ageSeconds)
}

// ObserveAgentRun records one committed agent run. Cost is added only when it
// is finite and positive; negative cost is rejected by the agent store before
// this point and is never observed.
func (m *Metrics) ObserveAgentRun(outcome, model string, costUSD float64) {
	outcome = normalizeOutcome(outcome)
	model = normalizeModel(model)
	m.agentRuns.WithLabelValues(outcome, model).Inc()
	cost := m.agentCost.WithLabelValues(model)
	if costUSD > 0 && finite(costUSD) {
		cost.Add(costUSD)
	}
}

// ObserveApprovalDecision records one committed, non-replayed human decision.
// Negative latency clamps to zero; non-finite latency is ignored.
func (m *Metrics) ObserveApprovalDecision(tier string, latencySeconds float64) {
	if !finite(latencySeconds) {
		return
	}
	if latencySeconds < 0 {
		latencySeconds = 0
	}
	m.approvalDelay.WithLabelValues(normalizeTier(tier)).Observe(latencySeconds)
}

// ObserveError increments a bounded component/code pair. Unknown components
// and codes fold to unknown.
func (m *Metrics) ObserveError(component, code string) {
	m.errors.WithLabelValues(normalizeComponent(component), normalizeCode(code)).Inc()
}

// ObserveOutboxDeadLetter increments the dead-letter counter for a topic.
func (m *Metrics) ObserveOutboxDeadLetter(topic string) {
	m.deadLetters.WithLabelValues(normalizeTopic(topic)).Inc()
}

// ObserveOutboxFailure records a bounded delivery failure. It never labels by
// message ID or raw error text.
func (m *Metrics) ObserveOutboxFailure() {
	m.errors.WithLabelValues("outbox", "delivery_failed").Inc()
}

// SetAutoApplyCapUsageRatio sets the clamped cap usage for a currency.
// Non-finite ratios are ignored; negative ratios clamp to zero and values
// above one clamp to one.
func (m *Metrics) SetAutoApplyCapUsageRatio(currency string, ratio float64) {
	if !finite(ratio) {
		return
	}
	ratio = math.Max(0, math.Min(1, ratio))
	m.capUsage.WithLabelValues(normalizeCurrency(currency)).Set(ratio)
}

// SetTieoutUnexplainedMinor sets the signed unexplained amount from a tie-out
// computation. Currency and source are normalized; the amount keeps its sign.
func (m *Metrics) SetTieoutUnexplainedMinor(currency, source string, minor int64) {
	m.tieout.WithLabelValues(normalizeCurrency(currency), normalizeSource(source)).Set(float64(minor))
}

// SetQueueDepth sets one closed queue gauge. Unknown queue names are ignored
// because queue has no unknown child; negative depths clamp to zero.
func (m *Metrics) SetQueueDepth(queue string, depth int64) {
	queue, ok := normalizeQueue(queue)
	if !ok {
		return
	}
	if depth < 0 {
		depth = 0
	}
	m.queueDepth.WithLabelValues(queue).Set(float64(depth))
}

// SetOutboxLag sets the oldest undelivered outbox age for a topic. Negative
// ages clamp to zero, non-finite ages are ignored, and invalid topics fold to
// unknown.
func (m *Metrics) SetOutboxLag(topic string, seconds float64) {
	if !finite(seconds) {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	m.outboxLag.WithLabelValues(normalizeTopic(topic)).Set(seconds)
}

// SetIntegrityJobStatus sets the integrity gauge. Values other than 0, 1, and
// 2 are ignored; M5-06 owns the producer.
func (m *Metrics) SetIntegrityJobStatus(job string, status int) {
	if status < 0 || status > 2 {
		return
	}
	m.integrity.WithLabelValues(normalizeEnum(job, jobValues)).Set(float64(status))
}

func (m *Metrics) seed() {
	m.buildInfo.WithLabelValues(m.component).Set(1)
	m.matchRuns.WithLabelValues(unknownLabelValue)
	m.matches.WithLabelValues(unknownLabelValue, unknownLabelValue)
	m.breaks.WithLabelValues(unknownLabelValue, unknownLabelValue)
	m.caseAge.WithLabelValues(unknownLabelValue)
	for _, queue := range queueValues {
		m.queueDepth.WithLabelValues(queue).Set(0)
	}
	m.agentRuns.WithLabelValues(unknownLabelValue, unknownLabelValue)
	m.agentCost.WithLabelValues(unknownLabelValue)
	m.approvalDelay.WithLabelValues(unknownLabelValue)
	m.errors.WithLabelValues(unknownLabelValue, unknownLabelValue)
	m.outboxLag.WithLabelValues(unknownLabelValue)
	m.deadLetters.WithLabelValues(unknownLabelValue)
	m.capUsage.WithLabelValues(unknownLabelValue)
	m.tieout.WithLabelValues(unknownLabelValue, unknownLabelValue)
	m.integrity.WithLabelValues(JobIntegrity).Set(0)
}

func newCounterVec(name string) *prometheus.CounterVec {
	spec := mustCatalogSpec(name)
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: spec.Name,
		Help: spec.Help,
	}, spec.labelNames())
}

func newGaugeVec(name string) *prometheus.GaugeVec {
	spec := mustCatalogSpec(name)
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: spec.Name,
		Help: spec.Help,
	}, spec.labelNames())
}

func newHistogramVec(name string) *prometheus.HistogramVec {
	spec := mustCatalogSpec(name)
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    spec.Name,
		Help:    spec.Help,
		Buckets: spec.Buckets,
	}, spec.labelNames())
}

func mustCatalogSpec(name string) MetricSpec {
	spec, ok := catalogSpec(name)
	if !ok {
		panic("metrics: missing catalog specification for " + name)
	}
	return spec
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
