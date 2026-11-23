package metrics

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// exemptMetricPrefixes are families owned by client_golang or its promhttp
// handler. Their names, help text, labels, and values are not part of the
// reconciliation contract, so ValidateRegistry documents and skips them.
var exemptMetricPrefixes = []string{"go_", "process_", "promhttp_"}

// labelKinds fixes each catalogued label name to one validator. A misspelled
// kind or a future label that bypasses the catalog fails construction.
var labelKinds = map[string]LabelKind{
	"source":    LabelSource,
	"currency":  LabelCurrency,
	"model":     LabelModel,
	"rule":      LabelEnum,
	"side":      LabelEnum,
	"status":    LabelEnum,
	"outcome":   LabelEnum,
	"tier":      LabelEnum,
	"queue":     LabelEnum,
	"topic":     LabelEnum,
	"job":       LabelEnum,
	"component": LabelEnum,
	"code":      LabelEnum,
}

// ValidateRegistry gathers a registry and enforces the frozen catalog contract:
// every recon_* family is present, its type/help/label order/buckets match,
// every value is allow-listed and safe, and no family exceeds its series cap.
// Unknown recon_* families are rejected except the documented go_, process_,
// and promhttp_ exemptions.
func ValidateRegistry(g prometheus.Gatherer) error {
	families, err := snapshotGatherer(g)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(families))
	for _, family := range families {
		if isExemptMetricFamily(family.name) {
			continue
		}
		if !strings.HasPrefix(family.name, "recon_") {
			return fmt.Errorf("metrics: unexpected non-recon metric family %q", family.name)
		}
		spec, ok := catalogSpec(family.name)
		if !ok {
			return fmt.Errorf("metrics: unknown recon metric family %q", family.name)
		}
		if _, duplicate := seen[family.name]; duplicate {
			return fmt.Errorf("metrics: duplicate metric family %q", family.name)
		}
		seen[family.name] = struct{}{}
		if err := validateFamilySnapshot(family, spec); err != nil {
			return fmt.Errorf("metrics: family %s: %w", family.name, err)
		}
	}

	for _, spec := range catalog {
		if _, ok := seen[spec.Name]; !ok {
			return fmt.Errorf("metrics: required family %s is missing", spec.Name)
		}
	}
	return nil
}

// RegisterChecked registers a collector after checking that its specification
// equals a frozen catalog family and that at least one emitted series matches
// that family's type, help, labels, values, and buckets. It is intended for
// collectors that are not constructed by New, such as scrape-time database
// collectors. Unknown recon_* families are rejected because ValidateRegistry
// would reject them as well.
func RegisterChecked(reg *prometheus.Registry, collector prometheus.Collector, spec MetricSpec) error {
	if reg == nil {
		return fmt.Errorf("metrics: nil registry")
	}
	if collector == nil {
		return fmt.Errorf("metrics: nil collector")
	}
	if err := validateSpec(spec); err != nil {
		return fmt.Errorf("metrics: invalid checked specification: %w", err)
	}
	cataloged, ok := catalogSpec(spec.Name)
	if !ok {
		return fmt.Errorf("metrics: %q is not in the frozen catalog", spec.Name)
	}
	if !specsEqual(spec, cataloged) {
		return fmt.Errorf("metrics: checked specification for %q does not match the frozen catalog", spec.Name)
	}

	// Validate the collector in an isolated registry first so a rejected
	// collector is never partially attached to the process registry.
	probe := prometheus.NewRegistry()
	if err := probe.Register(collector); err != nil {
		return fmt.Errorf("metrics: checking %s: %w", spec.Name, err)
	}
	families, err := snapshotGatherer(probe)
	if err != nil {
		return fmt.Errorf("metrics: checking %s: %w", spec.Name, err)
	}
	if len(families) != 1 {
		return fmt.Errorf("metrics: checked collector for %s emitted %d families, want exactly 1", spec.Name, len(families))
	}
	if families[0].name != spec.Name {
		return fmt.Errorf("metrics: checked collector emitted family %q, want %q", families[0].name, spec.Name)
	}
	if err := validateFamilySnapshot(families[0], cataloged); err != nil {
		return fmt.Errorf("metrics: checked collector for %s: %w", spec.Name, err)
	}

	if err := reg.Register(collector); err != nil {
		return fmt.Errorf("metrics: registering %s: %w", spec.Name, err)
	}
	return nil
}

func isExemptMetricFamily(name string) bool {
	for _, prefix := range exemptMetricPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// familySnapshot is a dependency-free view of gathered output, so the guard
// does not leak the DTO types into callers.
type familySnapshot struct {
	name   string
	help   string
	typ    string
	series []seriesSnapshot
}

type seriesSnapshot struct {
	labels       []labelSnapshot
	hasCounter   bool
	hasGauge     bool
	hasHistogram bool
	counter      float64
	gauge        float64
	count        uint64
	sum          float64
	buckets      []bucketSnapshot
}

type labelSnapshot struct {
	name  string
	value string
}

type bucketSnapshot struct {
	upper float64
	count uint64
}

// snapshotGatherer converts gathered DTO output into local structs. The DTO
// types stay inferred so client_model remains an indirect dependency.
func snapshotGatherer(g prometheus.Gatherer) ([]familySnapshot, error) {
	if g == nil {
		return nil, fmt.Errorf("metrics: nil gatherer")
	}
	families, err := g.Gather()
	if err != nil {
		return nil, fmt.Errorf("metrics: gathering: %w", err)
	}
	out := make([]familySnapshot, 0, len(families))
	for _, family := range families {
		snapshot := familySnapshot{
			name: family.GetName(),
			help: family.GetHelp(),
			typ:  strings.ToLower(family.GetType().String()),
		}
		for _, metric := range family.GetMetric() {
			series := seriesSnapshot{}
			for _, label := range metric.GetLabel() {
				series.labels = append(series.labels, labelSnapshot{
					name:  label.GetName(),
					value: label.GetValue(),
				})
			}
			if counter := metric.GetCounter(); counter != nil {
				series.hasCounter = true
				series.counter = counter.GetValue()
			}
			if gauge := metric.GetGauge(); gauge != nil {
				series.hasGauge = true
				series.gauge = gauge.GetValue()
			}
			if histogram := metric.GetHistogram(); histogram != nil {
				series.hasHistogram = true
				series.count = histogram.GetSampleCount()
				series.sum = histogram.GetSampleSum()
				for _, bucket := range histogram.GetBucket() {
					series.buckets = append(series.buckets, bucketSnapshot{
						upper: bucket.GetUpperBound(),
						count: bucket.GetCumulativeCount(),
					})
				}
			}
			snapshot.series = append(snapshot.series, series)
		}
		out = append(out, snapshot)
	}
	return out, nil
}

// validateCatalog checks every row before New registers anything.
func validateCatalog(specs []MetricSpec) error {
	names := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		if _, duplicate := names[spec.Name]; duplicate {
			return fmt.Errorf("metrics: duplicate catalog metric name %q", spec.Name)
		}
		names[spec.Name] = struct{}{}
		if err := validateSpec(spec); err != nil {
			return fmt.Errorf("metrics: catalog %s: %w", spec.Name, err)
		}
	}
	return nil
}

func validateSpec(spec MetricSpec) error {
	if !metricNameRE.MatchString(spec.Name) {
		return fmt.Errorf("name must match %s", metricNameRE)
	}
	switch spec.Type {
	case TypeCounter:
		if !strings.HasSuffix(spec.Name, "_total") {
			return fmt.Errorf("counter name must end in _total")
		}
	case TypeGauge, TypeHistogram:
		if strings.HasSuffix(spec.Name, "_total") {
			return fmt.Errorf("non-counter name must not end in _total")
		}
	default:
		return fmt.Errorf("unknown type %q", spec.Type)
	}
	if spec.Unit == "" {
		return fmt.Errorf("unit is required")
	}
	if spec.Help == "" || strings.TrimSpace(spec.Help) != spec.Help ||
		strings.ContainsAny(spec.Help, "\r\n") || !strings.HasSuffix(spec.Help, ".") {
		return fmt.Errorf("help must be one non-empty sentence ending in a period")
	}
	if spec.MaxSeries <= 0 {
		return fmt.Errorf("max series must be positive")
	}
	if spec.Type == TypeHistogram {
		if err := validateBuckets(spec.Buckets); err != nil {
			return err
		}
	} else if len(spec.Buckets) != 0 {
		return fmt.Errorf("only histograms may declare buckets")
	}

	labels := make(map[string]struct{}, len(spec.Labels))
	for _, label := range spec.Labels {
		if err := validateLabelName(label.Name); err != nil {
			return fmt.Errorf("label %q: %w", label.Name, err)
		}
		if _, duplicate := labels[label.Name]; duplicate {
			return fmt.Errorf("duplicate label %q", label.Name)
		}
		labels[label.Name] = struct{}{}
		expected, known := labelKinds[label.Name]
		if !known {
			return fmt.Errorf("label %q is not declared by the catalog guard", label.Name)
		}
		if label.Kind != expected {
			return fmt.Errorf("label %q has kind %q, want %q", label.Name, label.Kind, expected)
		}
		switch label.Kind {
		case LabelEnum:
			if err := validateEnumValues(spec.Name, label); err != nil {
				return err
			}
		case LabelSource, LabelCurrency, LabelModel:
			if len(label.Allowed) != 0 {
				return fmt.Errorf("label %q is open-ended and must not declare allowed values", label.Name)
			}
		default:
			return fmt.Errorf("label %q has unknown kind %q", label.Name, label.Kind)
		}
	}
	return nil
}

func validateEnumValues(metricName string, label LabelSpec) error {
	if len(label.Allowed) == 0 {
		return fmt.Errorf("label %q has an empty allow-list", label.Name)
	}
	seen := make(map[string]struct{}, len(label.Allowed))
	for _, value := range label.Allowed {
		if value == "" || !safeLabelValue(value) {
			return fmt.Errorf("label %q has unsafe allow-list value %q", label.Name, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("label %q has duplicate allow-list value %q", label.Name, value)
		}
		seen[value] = struct{}{}
	}
	_, hasUnknown := seen[unknownLabelValue]
	if !hasUnknown && !closedEnumAllowsNoUnknown(metricName, label.Name) {
		return fmt.Errorf("label %q must allow %q so invalid values can be normalized", label.Name, unknownLabelValue)
	}
	return nil
}

// closedEnumAllowsNoUnknown documents the two internal enums whose values are
// produced only by code and therefore have no unknown child.
func closedEnumAllowsNoUnknown(metricName, labelName string) bool {
	return (metricName == MetricBuildInfo && labelName == "component") ||
		(metricName == MetricQueueDepth && labelName == "queue")
}

func validateBuckets(buckets []float64) error {
	if len(buckets) == 0 {
		return fmt.Errorf("histogram requires buckets")
	}
	previous := 0.0
	for i, bucket := range buckets {
		if math.IsNaN(bucket) || math.IsInf(bucket, 0) {
			return fmt.Errorf("bucket %d is not finite", i)
		}
		if bucket <= 0 {
			return fmt.Errorf("bucket %d must be positive", i)
		}
		if i > 0 && bucket <= previous {
			return fmt.Errorf("buckets must be strictly increasing")
		}
		previous = bucket
	}
	return nil
}

func validateFamilySnapshot(family familySnapshot, spec MetricSpec) error {
	if family.name != spec.Name {
		return fmt.Errorf("name %q, want %q", family.name, spec.Name)
	}
	if family.help != spec.Help {
		return fmt.Errorf("help %q, want %q", family.help, spec.Help)
	}
	if family.typ != string(spec.Type) {
		return fmt.Errorf("type %q, want %q", family.typ, spec.Type)
	}
	if len(family.series) == 0 {
		return fmt.Errorf("family has no series")
	}
	if len(family.series) > spec.MaxSeries {
		return fmt.Errorf("series count %d exceeds cap %d", len(family.series), spec.MaxSeries)
	}

	seen := make(map[string]struct{}, len(family.series))
	for i, series := range family.series {
		if err := validateSeriesSnapshot(series, spec); err != nil {
			return fmt.Errorf("series %d: %w", i, err)
		}
		key := seriesKey(series)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("series %d duplicates label values", i)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateSeriesSnapshot(series seriesSnapshot, spec MetricSpec) error {
	if len(series.labels) != len(spec.Labels) {
		return fmt.Errorf("has %d labels, want %d", len(series.labels), len(spec.Labels))
	}
	// client_golang sorts gathered label pairs by name, so compare the label
	// set rather than the declaration order used by the catalog table.
	actual := make(map[string]string, len(series.labels))
	actualNames := make([]string, 0, len(series.labels))
	for _, label := range series.labels {
		if _, duplicate := actual[label.name]; duplicate {
			return fmt.Errorf("label %q appears more than once", label.name)
		}
		actual[label.name] = label.value
		actualNames = append(actualNames, label.name)
	}
	expectedNames := spec.labelNames()
	sort.Strings(actualNames)
	sort.Strings(expectedNames)
	if !slices.Equal(actualNames, expectedNames) {
		return fmt.Errorf("labels %v, want %v", actualNames, expectedNames)
	}
	for _, label := range spec.Labels {
		value := actual[label.Name]
		if !validateLabelValue(label, value) {
			return fmt.Errorf("label %q has invalid value %q", label.Name, value)
		}
	}

	switch spec.Type {
	case TypeCounter:
		if !series.hasCounter {
			return fmt.Errorf("counter sample is missing")
		}
	case TypeGauge:
		if !series.hasGauge {
			return fmt.Errorf("gauge sample is missing")
		}
	case TypeHistogram:
		if !series.hasHistogram {
			return fmt.Errorf("histogram sample is missing")
		}
		if err := validateHistogramBuckets(series.buckets, spec.Buckets); err != nil {
			return err
		}
	}
	return nil
}

func validateHistogramBuckets(got []bucketSnapshot, want []float64) error {
	// client_golang returns only the configured finite buckets; +Inf is
	// implicit in the histogram representation.
	if len(got) != len(want) {
		return fmt.Errorf("has %d buckets, want %d", len(got), len(want))
	}
	for i, upper := range want {
		if math.Abs(got[i].upper-upper) > 1e-9 {
			return fmt.Errorf("bucket %d upper bound %v, want %v", i, got[i].upper, upper)
		}
	}
	return nil
}

func seriesKey(series seriesSnapshot) string {
	var builder strings.Builder
	for _, label := range series.labels {
		builder.WriteString(label.name)
		builder.WriteByte('=')
		builder.WriteString(label.value)
		builder.WriteByte(0)
	}
	return builder.String()
}

func specsEqual(a, b MetricSpec) bool {
	if a.Name != b.Name || a.Type != b.Type || a.Unit != b.Unit ||
		a.Help != b.Help || a.MaxSeries != b.MaxSeries {
		return false
	}
	if !slices.Equal(a.Buckets, b.Buckets) {
		return false
	}
	if len(a.Labels) != len(b.Labels) {
		return false
	}
	for i := range a.Labels {
		if a.Labels[i].Name != b.Labels[i].Name ||
			a.Labels[i].Kind != b.Labels[i].Kind ||
			!slices.Equal(a.Labels[i].Allowed, b.Labels[i].Allowed) {
			return false
		}
	}
	return true
}
