package metrics

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// familyView and its siblings are a test-local view of gathered output. They
// keep the tests free of client_model imports.
type familyView struct {
	name   string
	help   string
	typ    string
	series []seriesView
}

type seriesView struct {
	labels    []labelView
	counter   *float64
	gauge     *float64
	histogram *histogramView
}

type histogramView struct {
	count   uint64
	sum     float64
	buckets []bucketView
}

type bucketView struct {
	upper float64
	count uint64
}

type labelView struct {
	name  string
	value string
}

func gatherViews(t *testing.T, gatherer prometheus.Gatherer) map[string]*familyView {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	views := make(map[string]*familyView, len(families))
	for _, family := range families {
		view := &familyView{
			name: family.GetName(),
			help: family.GetHelp(),
			typ:  strings.ToLower(family.GetType().String()),
		}
		for _, metric := range family.GetMetric() {
			series := seriesView{}
			for _, label := range metric.GetLabel() {
				series.labels = append(series.labels, labelView{
					name:  label.GetName(),
					value: label.GetValue(),
				})
			}
			if counter := metric.GetCounter(); counter != nil {
				value := counter.GetValue()
				series.counter = &value
			}
			if gauge := metric.GetGauge(); gauge != nil {
				value := gauge.GetValue()
				series.gauge = &value
			}
			if histogram := metric.GetHistogram(); histogram != nil {
				viewHistogram := &histogramView{
					count: histogram.GetSampleCount(),
					sum:   histogram.GetSampleSum(),
				}
				for _, bucket := range histogram.GetBucket() {
					viewHistogram.buckets = append(viewHistogram.buckets, bucketView{
						upper: bucket.GetUpperBound(),
						count: bucket.GetCumulativeCount(),
					})
				}
				series.histogram = viewHistogram
			}
			view.series = append(view.series, series)
		}
		views[view.name] = view
	}
	return views
}

func mustFamily(t *testing.T, views map[string]*familyView, name string) *familyView {
	t.Helper()
	family, ok := views[name]
	if !ok {
		t.Fatalf("metric family %s is missing", name)
	}
	return family
}

func mustSeries(t *testing.T, family *familyView, pairs map[string]string) *seriesView {
	t.Helper()
	for i := range family.series {
		series := &family.series[i]
		if len(series.labels) != len(pairs) {
			continue
		}
		matches := true
		for _, label := range series.labels {
			if pairs[label.name] != label.value {
				matches = false
				break
			}
		}
		if matches {
			return series
		}
	}
	t.Fatalf("family %s has no series with labels %v; got %v", family.name, pairs, family.series)
	return nil
}

func mustNew(t *testing.T, opts Options) *Metrics {
	t.Helper()
	metrics, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return metrics
}

func mustSpec(t *testing.T, name string) MetricSpec {
	t.Helper()
	spec, ok := catalogSpec(name)
	if !ok {
		t.Fatalf("catalog is missing %s", name)
	}
	return spec
}

func counterValue(t *testing.T, series *seriesView) float64 {
	t.Helper()
	if series.counter == nil {
		t.Fatal("series is not a counter")
	}
	return *series.counter
}

func gaugeValue(t *testing.T, series *seriesView) float64 {
	t.Helper()
	if series.gauge == nil {
		t.Fatal("series is not a gauge")
	}
	return *series.gauge
}

func histogramValue(t *testing.T, series *seriesView) *histogramView {
	t.Helper()
	if series.histogram == nil {
		t.Fatal("series is not a histogram")
	}
	return series.histogram
}

func TestCatalogFrozenContract(t *testing.T) {
	want := []struct {
		name      string
		typ       Type
		unit      string
		help      string
		labels    []string
		buckets   []float64
		maxSeries int
	}{
		{
			MetricBuildInfo, TypeGauge, "info",
			"Build information for this process; the value is always 1.",
			[]string{"component"}, nil, 3,
		},
		{
			MetricMatchRunsTotal, TypeCounter, "runs",
			"Total number of matcher runs that committed successfully.",
			[]string{"source"}, nil, 32,
		},
		{
			MetricMatchesTotal, TypeCounter, "matches",
			"Total number of accepted matches produced by matcher runs, by rule.",
			[]string{"source", "rule"}, nil, 64,
		},
		{
			MetricBreaksTotal, TypeCounter, "breaks",
			"Total number of reconciliation breaks detected, by side.",
			[]string{"source", "side"}, nil, 32,
		},
		{
			MetricCaseAgeSeconds, TypeHistogram, "seconds",
			"Age in seconds of a reconciliation case when its status was observed.",
			[]string{"status"}, []float64{
				30, 300, 900, 3600, 21600, 43200, 86400, 172800, 259200,
				604800, 2592000,
			}, 8,
		},
		{
			MetricQueueDepth, TypeGauge, "items",
			"Number of items currently waiting in a queue.",
			[]string{"queue"}, nil, 3,
		},
		{
			MetricAgentRunsTotal, TypeCounter, "runs",
			"Total number of agent runs that finished, by outcome and model.",
			[]string{"outcome", "model"}, nil, 64,
		},
		{
			MetricAgentCostUSDTotal, TypeCounter, "USD",
			"Total agent cost in US dollars, by model.",
			[]string{"model"}, nil, 32,
		},
		{
			MetricApprovalLatencySeconds, TypeHistogram, "seconds",
			"Latency in seconds from proposal creation to a human approval decision, by risk tier.",
			[]string{"tier"}, []float64{
				30, 60, 300, 900, 1800, 3600, 14400, 86400, 259200,
			}, 4,
		},
		{
			MetricErrorsTotal, TypeCounter, "errors",
			"Total number of bounded errors, by component and code.",
			[]string{"component", "code"}, nil, 64,
		},
		{
			MetricOutboxLagSeconds, TypeGauge, "seconds",
			"Age in seconds of the oldest undelivered outbox message, by topic.",
			[]string{"topic"}, nil, 4,
		},
		{
			MetricOutboxDeadLettersTotal, TypeCounter, "messages",
			"Total number of outbox messages that reached the dead-letter state, by topic.",
			[]string{"topic"}, nil, 4,
		},
		{
			MetricAutoApplyCapUsageRatio, TypeGauge, "ratio 0-1",
			"Ratio of the daily auto-apply cap currently in use, clamped to the range 0 to 1.",
			[]string{"currency"}, nil, 32,
		},
		{
			MetricTieoutUnexplainedMinor, TypeGauge, "minor units",
			"Signed unexplained amount in minor currency units from the latest tie-out computation.",
			[]string{"currency", "source"}, nil, 64,
		},
		{
			MetricIntegrityJobStatus, TypeGauge, "status",
			"Integrity job status: 0=unknown/not run, 1=pass, 2=fail.",
			[]string{"job"}, nil, 2,
		},
	}

	if len(catalog) != len(want) {
		t.Fatalf("catalog has %d families, want %d", len(catalog), len(want))
	}
	for i, expected := range want {
		got := catalog[i]
		if got.Name != expected.name || got.Type != expected.typ ||
			got.Unit != expected.unit || got.Help != expected.help ||
			got.MaxSeries != expected.maxSeries {
			t.Errorf("catalog[%d] = %s/%s/%q/%q/cap %d, want %s/%s/%q/%q/cap %d",
				i, got.Name, got.Type, got.Unit, got.Help, got.MaxSeries,
				expected.name, expected.typ, expected.unit, expected.help, expected.maxSeries)
		}
		var labels []string
		for _, label := range got.Labels {
			labels = append(labels, label.Name)
		}
		if strings.Join(labels, ",") != strings.Join(expected.labels, ",") {
			t.Errorf("%s labels = %v, want %v", got.Name, labels, expected.labels)
		}
		if len(got.Buckets) != len(expected.buckets) {
			t.Errorf("%s buckets = %v, want %v", got.Name, got.Buckets, expected.buckets)
		} else {
			for i, upper := range expected.buckets {
				if math.Abs(got.Buckets[i]-upper) > 1e-9 {
					t.Errorf("%s bucket %d = %v, want %v", got.Name, i, got.Buckets[i], upper)
				}
			}
		}
	}
}

func TestCatalogLabelAllowLists(t *testing.T) {
	type labelContract struct {
		kind    LabelKind
		allowed []string
	}
	want := map[string]labelContract{
		MetricBuildInfo + ".component": {
			LabelEnum, []string{"api", "worker", "outbox"},
		},
		MetricMatchRunsTotal + ".source": {LabelSource, nil},
		MetricMatchesTotal + ".source":   {LabelSource, nil},
		MetricMatchesTotal + ".rule": {
			LabelEnum, []string{
				"exact", "reference_tolerance", "composite_batch", "composite",
				"fuzzy", "duplicate", "unknown",
			},
		},
		MetricBreaksTotal + ".source": {LabelSource, nil},
		MetricBreaksTotal + ".side": {
			LabelEnum, []string{
				"ledger_only", "external_only", "amount_mismatch", "duplicate",
				"unknown",
			},
		},
		MetricCaseAgeSeconds + ".status": {
			LabelEnum, []string{
				"open", "in_agent", "proposal_pending", "waiting_info",
				"escalated", "resolved", "unknown",
			},
		},
		MetricQueueDepth + ".queue": {
			LabelEnum, []string{"cases_open", "approvals_pending", "outbox_pending"},
		},
		MetricAgentRunsTotal + ".outcome": {
			LabelEnum, []string{
				"proposal", "escalated", "budget_exhausted", "invalid_output",
				"provider_error", "unknown",
			},
		},
		MetricAgentRunsTotal + ".model":    {LabelModel, nil},
		MetricAgentCostUSDTotal + ".model": {LabelModel, nil},
		MetricApprovalLatencySeconds + ".tier": {
			LabelEnum, []string{"T0", "T1", "T2", "unknown"},
		},
		MetricErrorsTotal + ".component": {
			LabelEnum, []string{
				"api", "worker", "agent", "matcher", "approval", "limits",
				"outbox", "tieout", "metrics", "integrity", "unknown",
			},
		},
		MetricErrorsTotal + ".code": {
			LabelEnum, []string{
				"internal", "invalid_request", "not_found", "conflict",
				"unauthorized", "forbidden", "timeout", "unavailable",
				"provider_error", "invalid_output", "delivery_failed",
				"dead_letter", "collect", "daily_cap_exceeded", "unknown",
			},
		},
		MetricOutboxLagSeconds + ".topic": {
			LabelEnum, []string{
				"approval.request", "approval.update", "approval.cancel",
				"unknown",
			},
		},
		MetricOutboxDeadLettersTotal + ".topic": {
			LabelEnum, []string{
				"approval.request", "approval.update", "approval.cancel",
				"unknown",
			},
		},
		MetricAutoApplyCapUsageRatio + ".currency": {LabelCurrency, nil},
		MetricTieoutUnexplainedMinor + ".currency": {LabelCurrency, nil},
		MetricTieoutUnexplainedMinor + ".source":   {LabelSource, nil},
		MetricIntegrityJobStatus + ".job": {
			LabelEnum, []string{"integrity", "unknown"},
		},
	}

	found := 0
	for _, spec := range catalog {
		for _, label := range spec.Labels {
			key := spec.Name + "." + label.Name
			expected, ok := want[key]
			if !ok {
				t.Errorf("catalog label %s is not in the contract test", key)
				continue
			}
			found++
			if label.Kind != expected.kind {
				t.Errorf("%s kind = %s, want %s", key, label.Kind, expected.kind)
			}
			if strings.Join(label.Allowed, ",") != strings.Join(expected.allowed, ",") {
				t.Errorf("%s allowed = %v, want %v", key, label.Allowed, expected.allowed)
			}
		}
	}
	if found != len(want) {
		t.Errorf("checked %d labels, want %d", found, len(want))
	}
}

func TestNewExposesEveryFrozenFamily(t *testing.T) {
	metrics := mustNew(t, Options{Component: "api"})
	views := gatherViews(t, metrics.Registry())
	if len(views) != len(catalog) {
		t.Fatalf("gathered %d families, want %d", len(views), len(catalog))
	}

	for _, spec := range catalog {
		family := mustFamily(t, views, spec.Name)
		if family.typ != string(spec.Type) {
			t.Errorf("%s type = %s, want %s", spec.Name, family.typ, spec.Type)
		}
		if family.help != spec.Help {
			t.Errorf("%s help = %q, want %q", spec.Name, family.help, spec.Help)
		}
		if len(family.series) == 0 {
			t.Errorf("%s has no series", spec.Name)
		}
		wantNames := spec.labelNames()
		sort.Strings(wantNames)
		for _, series := range family.series {
			if len(series.labels) != len(spec.Labels) {
				t.Errorf("%s series has %d labels, want %d", spec.Name, len(series.labels), len(spec.Labels))
				continue
			}
			gotNames := make([]string, 0, len(series.labels))
			for _, label := range series.labels {
				gotNames = append(gotNames, label.name)
			}
			sort.Strings(gotNames)
			if !slices.Equal(gotNames, wantNames) {
				t.Errorf("%s labels = %v, want %v", spec.Name, gotNames, wantNames)
			}
		}
	}

	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSeededSeries(t *testing.T) {
	metrics := mustNew(t, Options{Component: "worker"})
	views := gatherViews(t, metrics.Registry())
	unknown := unknownLabelValue

	build := mustFamily(t, views, MetricBuildInfo)
	if len(build.series) != 1 {
		t.Fatalf("build info has %d series, want 1", len(build.series))
	}
	buildSeries := mustSeries(t, build, map[string]string{"component": "worker"})
	if got := gaugeValue(t, buildSeries); got != 1 {
		t.Errorf("build info = %v, want 1", got)
	}

	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricMatchRunsTotal), map[string]string{"source": unknown})); got != 0 {
		t.Errorf("match runs seed = %v, want 0", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricMatchesTotal), map[string]string{"source": unknown, "rule": unknown})); got != 0 {
		t.Errorf("matches seed = %v, want 0", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricBreaksTotal), map[string]string{"source": unknown, "side": unknown})); got != 0 {
		t.Errorf("breaks seed = %v, want 0", got)
	}
	if got := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricCaseAgeSeconds), map[string]string{"status": unknown})); got.count != 0 || got.sum != 0 {
		t.Errorf("case age seed = count %d sum %v, want zero observations", got.count, got.sum)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentRunsTotal), map[string]string{"outcome": unknown, "model": unknown})); got != 0 {
		t.Errorf("agent runs seed = %v, want 0", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentCostUSDTotal), map[string]string{"model": unknown})); got != 0 {
		t.Errorf("agent cost seed = %v, want 0", got)
	}
	if got := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricApprovalLatencySeconds), map[string]string{"tier": unknown})); got.count != 0 || got.sum != 0 {
		t.Errorf("approval latency seed = count %d sum %v, want zero observations", got.count, got.sum)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricErrorsTotal), map[string]string{"component": unknown, "code": unknown})); got != 0 {
		t.Errorf("errors seed = %v, want 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricOutboxLagSeconds), map[string]string{"topic": unknown})); got != 0 {
		t.Errorf("outbox lag seed = %v, want 0", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricOutboxDeadLettersTotal), map[string]string{"topic": unknown})); got != 0 {
		t.Errorf("dead letters seed = %v, want 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricAutoApplyCapUsageRatio), map[string]string{"currency": unknown})); got != 0 {
		t.Errorf("cap ratio seed = %v, want 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricTieoutUnexplainedMinor), map[string]string{"currency": unknown, "source": unknown})); got != 0 {
		t.Errorf("tie-out seed = %v, want 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricIntegrityJobStatus), map[string]string{"job": JobIntegrity})); got != 0 {
		t.Errorf("integrity seed = %v, want 0", got)
	}

	queue := mustFamily(t, views, MetricQueueDepth)
	if len(queue.series) != len(queueValues) {
		t.Fatalf("queue depth has %d series, want %d", len(queue.series), len(queueValues))
	}
	for _, name := range queueValues {
		if got := gaugeValue(t, mustSeries(t, queue, map[string]string{"queue": name})); got != 0 {
			t.Errorf("queue %s = %v, want 0", name, got)
		}
	}

	caseAge := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricCaseAgeSeconds), map[string]string{"status": unknown}))
	if len(caseAge.buckets) != len(caseAgeBuckets) {
		t.Errorf("case age buckets = %v, want %v", caseAge.buckets, caseAgeBuckets)
	}
	for i, upper := range caseAgeBuckets {
		if math.Abs(caseAge.buckets[i].upper-upper) > 1e-9 {
			t.Errorf("case age bucket %d = %v, want %v", i, caseAge.buckets[i].upper, upper)
		}
	}
	approval := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricApprovalLatencySeconds), map[string]string{"tier": unknown}))
	if len(approval.buckets) != len(approvalLatencyBuckets) {
		t.Errorf("approval buckets = %v, want %v", approval.buckets, approvalLatencyBuckets)
	}
	for i, upper := range approvalLatencyBuckets {
		if math.Abs(approval.buckets[i].upper-upper) > 1e-9 {
			t.Errorf("approval bucket %d = %v, want %v", i, approval.buckets[i].upper, upper)
		}
	}
}

func TestGoAndProcessCollectors(t *testing.T) {
	metrics := mustNew(t, Options{Component: "worker", GoCollector: true, ProcessCollector: true})
	views := gatherViews(t, metrics.Registry())

	if _, ok := views["go_goroutines"]; !ok {
		t.Error("go_goroutines family is missing when the Go collector is enabled")
	}
	hasProcess := false
	for name := range views {
		if strings.HasPrefix(name, "process_") {
			hasProcess = true
			break
		}
	}
	if !hasProcess && (runtime.GOOS == "linux" || runtime.GOOS == "windows" || runtime.GOOS == "darwin") {
		t.Error("no process_* family when the process collector is enabled")
	}
	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate with exempt collectors: %v", err)
	}
}

func TestRegistryIsolationAndNoDefaultPollution(t *testing.T) {
	first := mustNew(t, Options{Component: "api"})
	second := mustNew(t, Options{Component: "api"})

	first.ObserveMatchRun(unknownLabelValue, nil, nil)
	firstViews := gatherViews(t, first.Registry())
	secondViews := gatherViews(t, second.Registry())
	if got := counterValue(t, mustSeries(t, mustFamily(t, firstViews, MetricMatchRunsTotal), map[string]string{"source": unknownLabelValue})); got != 1 {
		t.Errorf("first match runs = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, secondViews, MetricMatchRunsTotal), map[string]string{"source": unknownLabelValue})); got != 0 {
		t.Errorf("second match runs = %v, want 0", got)
	}

	defaultFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("default gather: %v", err)
	}
	for _, family := range defaultFamilies {
		if strings.HasPrefix(family.GetName(), "recon_") {
			t.Errorf("default registry contains recon family %s", family.GetName())
		}
	}
}

func TestNewRejectsInvalidComponent(t *testing.T) {
	for _, component := range []string{"", "cron", "API", "unknown"} {
		if _, err := New(Options{Component: component}); err == nil {
			t.Errorf("New accepted component %q", component)
		}
	}
}

func TestHelpersInstrumentAndNormalize(t *testing.T) {
	metrics := mustNew(t, Options{Component: "api"})
	const (
		uuid     = "123e4567-e89b-12d3-a456-426614174000"
		freeText = "Alice <alice@example.com>"
	)
	unknown := unknownLabelValue

	metrics.ObserveMatchRun(uuid, map[string]int{
		"mystery_rule": 2,
		"exact":        1,
		"fuzzy":        0,
		"duplicate":    -3,
	}, map[string]int{
		"mystery_side": 3,
		"duplicate":    1,
	})
	metrics.ObserveCaseAge("nonsense", -5)
	metrics.ObserveCaseAge("open", 10)
	metrics.ObserveAgentRun("nonsense", uuid, 1.25)
	metrics.ObserveAgentRun("proposal", "model-x", -1)
	metrics.ObserveApprovalDecision("T9", 12)
	metrics.ObserveError("case", "payload")
	metrics.ObserveOutboxDeadLetter(freeText)
	metrics.ObserveOutboxFailure()
	metrics.SetAutoApplyCapUsageRatio("usd", 2.5)
	metrics.SetAutoApplyCapUsageRatio("USD", -1)
	metrics.SetTieoutUnexplainedMinor("usd", uuid, -123)
	metrics.SetQueueDepth("bogus", 5)
	metrics.SetQueueDepth(QueueCasesOpen, 4)
	metrics.SetOutboxLag(freeText, -1)
	metrics.SetOutboxLag("approval.request", 2)
	metrics.SetIntegrityJobStatus("bogus", 9)
	metrics.SetIntegrityJobStatus(JobIntegrity, 2)

	views := gatherViews(t, metrics.Registry())
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricMatchRunsTotal), map[string]string{"source": unknown})); got != 1 {
		t.Errorf("match runs = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricMatchesTotal), map[string]string{"source": unknown, "rule": unknown})); got != 2 {
		t.Errorf("matches unknown/unknown = %v, want 2", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricMatchesTotal), map[string]string{"source": unknown, "rule": "exact"})); got != 1 {
		t.Errorf("matches unknown/exact = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricBreaksTotal), map[string]string{"source": unknown, "side": unknown})); got != 3 {
		t.Errorf("breaks unknown/unknown = %v, want 3", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricBreaksTotal), map[string]string{"source": unknown, "side": "duplicate"})); got != 1 {
		t.Errorf("breaks unknown/duplicate = %v, want 1", got)
	}

	caseAge := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricCaseAgeSeconds), map[string]string{"status": unknown}))
	if caseAge.count != 1 || caseAge.sum != 0 || caseAge.buckets[0].count != 1 {
		t.Errorf("unknown case age = count %d sum %v bucket0 %d, want count 1 at zero",
			caseAge.count, caseAge.sum, caseAge.buckets[0].count)
	}
	openCaseAge := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricCaseAgeSeconds), map[string]string{"status": "open"}))
	if openCaseAge.count != 1 || openCaseAge.sum != 10 {
		t.Errorf("open case age = count %d sum %v, want count 1 sum 10", openCaseAge.count, openCaseAge.sum)
	}

	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentRunsTotal), map[string]string{"outcome": unknown, "model": unknown})); got != 1 {
		t.Errorf("agent unknown run = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentCostUSDTotal), map[string]string{"model": unknown})); got != 1.25 {
		t.Errorf("agent unknown cost = %v, want 1.25", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentRunsTotal), map[string]string{"outcome": "proposal", "model": "model-x"})); got != 1 {
		t.Errorf("agent proposal run = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricAgentCostUSDTotal), map[string]string{"model": "model-x"})); got != 0 {
		t.Errorf("agent negative cost = %v, want 0", got)
	}

	if got := histogramValue(t, mustSeries(t, mustFamily(t, views, MetricApprovalLatencySeconds), map[string]string{"tier": unknown})).count; got != 1 {
		t.Errorf("approval unknown count = %d, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricErrorsTotal), map[string]string{"component": unknown, "code": unknown})); got != 1 {
		t.Errorf("errors unknown/unknown = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricErrorsTotal), map[string]string{"component": "outbox", "code": "delivery_failed"})); got != 1 {
		t.Errorf("outbox delivery failure = %v, want 1", got)
	}
	if got := counterValue(t, mustSeries(t, mustFamily(t, views, MetricOutboxDeadLettersTotal), map[string]string{"topic": unknown})); got != 1 {
		t.Errorf("dead letters unknown = %v, want 1", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricAutoApplyCapUsageRatio), map[string]string{"currency": unknown})); got != 1 {
		t.Errorf("cap ratio unknown = %v, want clamped 1", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricAutoApplyCapUsageRatio), map[string]string{"currency": "USD"})); got != 0 {
		t.Errorf("cap ratio USD = %v, want clamped 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricTieoutUnexplainedMinor), map[string]string{"currency": unknown, "source": unknown})); got != -123 {
		t.Errorf("tie-out unknown = %v, want -123", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricQueueDepth), map[string]string{"queue": QueueCasesOpen})); got != 4 {
		t.Errorf("queue cases_open = %v, want 4", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricOutboxLagSeconds), map[string]string{"topic": unknown})); got != 0 {
		t.Errorf("outbox lag unknown = %v, want 0", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricOutboxLagSeconds), map[string]string{"topic": "approval.request"})); got != 2 {
		t.Errorf("outbox lag approval.request = %v, want 2", got)
	}
	if got := gaugeValue(t, mustSeries(t, mustFamily(t, views, MetricIntegrityJobStatus), map[string]string{"job": JobIntegrity})); got != 2 {
		t.Errorf("integrity status = %v, want 2", got)
	}

	for _, family := range views {
		if !strings.HasPrefix(family.name, "recon_") {
			continue
		}
		for _, series := range family.series {
			for _, label := range series.labels {
				for _, forbidden := range []string{uuid, freeText, "mystery_rule", "mystery_side", "nonsense", "alice", "bogus"} {
					if strings.Contains(strings.ToLower(label.value), strings.ToLower(forbidden)) {
						t.Errorf("%s label %s contains forbidden value %q: %q", family.name, label.name, forbidden, label.value)
					}
				}
				if strings.Contains(label.value, "@") || strings.Contains(label.value, " ") {
					t.Errorf("%s label %s contains free text %q", family.name, label.name, label.value)
				}
			}
		}
	}

	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate after instrumentation: %v", err)
	}
}

func TestSeriesBounds(t *testing.T) {
	idle := mustNew(t, Options{Component: "api"})
	idleViews := gatherViews(t, idle.Registry())
	if got := countReconSeries(idleViews); got > 64 {
		t.Errorf("idle recon series = %d, want <= 64", got)
	}

	metrics := mustNew(t, Options{Component: "api"})
	for i := 0; i < 4; i++ {
		source := fmt.Sprintf("source_%d", i)
		metrics.ObserveMatchRun(source,
			map[string]int{
				"exact": 1, "reference_tolerance": 2, "composite_batch": 3,
				"composite": 4, "fuzzy": 5, "duplicate": 6, "unknown": 7,
			},
			map[string]int{
				"ledger_only": 1, "external_only": 2, "amount_mismatch": 3,
				"duplicate": 4, "unknown": 5,
			})
	}
	outcomes := []string{"proposal", "escalated", "budget_exhausted", "invalid_output", "provider_error", "unknown"}
	for i := 0; i < 4; i++ {
		model := fmt.Sprintf("model-%d", i)
		for _, outcome := range outcomes {
			metrics.ObserveAgentRun(outcome, model, 1)
		}
	}
	for _, currency := range []string{"USD", "EUR", "GBP", "JPY"} {
		metrics.SetAutoApplyCapUsageRatio(currency, 0.5)
		metrics.SetTieoutUnexplainedMinor(currency, "source_0", 100)
	}
	for _, status := range []string{"open", "escalated"} {
		metrics.ObserveCaseAge(status, 60)
	}
	for _, tier := range []string{"T0", "T1", "T2"} {
		metrics.ObserveApprovalDecision(tier, 60)
	}
	for _, topic := range []string{"approval.request", "approval.update"} {
		metrics.SetOutboxLag(topic, 1)
		metrics.ObserveOutboxDeadLetter(topic)
	}
	for i := 0; i < 4; i++ {
		metrics.ObserveError("matcher", codeValues[i])
	}

	views := gatherViews(t, metrics.Registry())
	if got := countReconSeries(views); got > 128 {
		t.Errorf("instrumented recon series = %d, want <= 128", got)
	}
	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate instrumented registry: %v", err)
	}
}

func countReconSeries(views map[string]*familyView) int {
	total := 0
	for name, family := range views {
		if strings.HasPrefix(name, "recon_") {
			total += len(family.series)
		}
	}
	return total
}
