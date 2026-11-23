package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// staticCollector emits exactly one constant metric. It is used to prove that
// the registration guard rejects forbidden labels and free-text values.
type staticCollector struct {
	desc        *prometheus.Desc
	valueType   prometheus.ValueType
	value       float64
	labelValues []string
}

func (c *staticCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *staticCollector) Collect(ch chan<- prometheus.Metric) {
	metric, err := prometheus.NewConstMetric(c.desc, c.valueType, c.value, c.labelValues...)
	if err != nil {
		panic(err)
	}
	ch <- metric
}

type emptyCollector struct {
	desc *prometheus.Desc
}

func (c *emptyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *emptyCollector) Collect(chan<- prometheus.Metric) {}

func TestForbiddenLabelNames(t *testing.T) {
	forbidden := []string{
		"case_id", "run_id", "agent_run_id", "proposal_id", "break_id",
		"scope_id", "ledger_txn_id", "approver_id", "trace_id",
		"idempotency_key", "entity_id", "message_id", "external_ref",
		"payload", "error", "reason", "rationale", "dsn", "url", "path",
		"email", "match_id", "outbox_id", "idempotency_id", "uuid", "guid",
		"customer_id", "job_id",
	}
	for _, name := range forbidden {
		if !isForbiddenLabelName(name) {
			t.Errorf("label %q was not rejected as forbidden", name)
		}
		if err := validateLabelName(name); err == nil {
			t.Errorf("validateLabelName(%q) = nil, want an error", name)
		}
	}

	for _, name := range []string{
		"source", "currency", "model", "rule", "side", "status", "outcome",
		"tier", "queue", "topic", "job", "component", "code",
	} {
		if isForbiddenLabelName(name) {
			t.Errorf("safe label %q was rejected as forbidden", name)
		}
	}
	if err := validateLabelName("le"); err == nil {
		t.Error("reserved label le was accepted")
	}
	if err := validateLabelName("Bad-Name"); err == nil {
		t.Error("invalid label syntax was accepted")
	}

	for _, spec := range catalog {
		for _, label := range spec.Labels {
			if isForbiddenLabelName(label.Name) {
				t.Errorf("catalog family %s contains forbidden label %s", spec.Name, label.Name)
			}
		}
	}
}

func TestNormalizeSource(t *testing.T) {
	valid := []string{"bank_ledger", "source1", "a", unknownLabelValue}
	for _, value := range valid {
		if got := normalizeSource(value); got != value {
			t.Errorf("normalizeSource(%q) = %q, want %q", value, got, value)
		}
	}
	invalid := []string{
		"", "Bank_Ledger", "bank-ledger", strings.Repeat("a", 33),
		"123e4567-e89b-12d3-a456-426614174000", "with space",
		"0123456789abcdef",
	}
	for _, value := range invalid {
		if got := normalizeSource(value); got != unknownLabelValue {
			t.Errorf("normalizeSource(%q) = %q, want unknown", value, got)
		}
	}
}

func TestNormalizeCurrency(t *testing.T) {
	for _, value := range []string{"USD", "EUR", "GBP", unknownLabelValue} {
		if got := normalizeCurrency(value); got != value {
			t.Errorf("normalizeCurrency(%q) = %q, want %q", value, got, value)
		}
	}
	for _, value := range []string{"", "usd", "US", "USDD", "U5D", " USD"} {
		if got := normalizeCurrency(value); got != unknownLabelValue {
			t.Errorf("normalizeCurrency(%q) = %q, want unknown", value, got)
		}
	}
}

func TestNormalizeModel(t *testing.T) {
	valid := []string{
		"deepseek-v4.1-flash",
		"openai/gpt-5",
		"anthropic:claude",
		"model.with_underscore",
		unknownLabelValue,
		strings.Repeat("m", 64),
	}
	for _, value := range valid {
		if got := normalizeModel(value); got != value {
			t.Errorf("normalizeModel(%q) = %q, want %q", value, got, value)
		}
	}
	invalid := []string{
		"",
		strings.Repeat("a", 65),
		"model with space",
		"https://example.com/model",
		"123e4567-e89b-12d3-a456-426614174000",
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"0123456789abcdef",
		"model\tname",
	}
	for _, value := range invalid {
		if got := normalizeModel(value); got != unknownLabelValue {
			t.Errorf("normalizeModel(%q) = %q, want unknown", value, got)
		}
	}
}

func TestNormalizeEnums(t *testing.T) {
	tests := []struct {
		name      string
		normalize func(string) string
		allowed   []string
	}{
		{"rule", normalizeRule, ruleValues},
		{"side", normalizeSide, sideValues},
		{"status", normalizeStatus, statusValues},
		{"outcome", normalizeOutcome, outcomeValues},
		{"tier", normalizeTier, tierValues},
		{"topic", normalizeTopic, topicValues},
		{"component", normalizeComponent, componentValues},
		{"code", normalizeCode, codeValues},
	}
	for _, test := range tests {
		for _, value := range test.allowed {
			if got := test.normalize(value); got != value {
				t.Errorf("normalize %s(%q) = %q, want %q", test.name, value, got, value)
			}
		}
		for _, value := range []string{
			"", "mystery", "123e4567-e89b-12d3-a456-426614174000",
			"free text", "EXACT",
		} {
			if got := test.normalize(value); got != unknownLabelValue {
				t.Errorf("normalize %s(%q) = %q, want unknown", test.name, value, got)
			}
		}
	}

	for _, value := range queueValues {
		if got, ok := normalizeQueue(value); !ok || got != value {
			t.Errorf("normalizeQueue(%q) = %q/%v, want %q/true", value, got, ok, value)
		}
	}
	for _, value := range []string{"", "mystery", unknownLabelValue, "cases"} {
		if _, ok := normalizeQueue(value); ok {
			t.Errorf("normalizeQueue(%q) accepted an invalid queue", value)
		}
	}
}

func TestLabelValuesStayBounded(t *testing.T) {
	for _, spec := range catalog {
		for _, label := range spec.Labels {
			if label.Kind == LabelEnum {
				for _, value := range label.Allowed {
					if !validateLabelValue(label, value) {
						t.Errorf("%s{%s=%q} rejected its own allow-list value", spec.Name, label.Name, value)
					}
				}
				if !closedEnumAllowsNoUnknown(spec.Name, label.Name) {
					if !validateLabelValue(label, unknownLabelValue) {
						t.Errorf("%s{%s=unknown} rejected the fallback", spec.Name, label.Name)
					}
				}
			}
		}
	}

	sourceSpec := LabelSpec{Name: "source", Kind: LabelSource}
	for _, value := range []string{"bank_ledger", unknownLabelValue} {
		if !validateLabelValue(sourceSpec, value) {
			t.Errorf("source value %q was rejected", value)
		}
	}
	for _, value := range []string{
		"", "Bank", "bank ledger", "123e4567-e89b-12d3-a456-426614174000",
		"https://example.com", " padded", "line\nbreak",
	} {
		if validateLabelValue(sourceSpec, value) {
			t.Errorf("source value %q was accepted", value)
		}
	}

	modelSpec := LabelSpec{Name: "model", Kind: LabelModel}
	for _, value := range []string{"openai/gpt-5", "deepseek-v4.1-flash"} {
		if !validateLabelValue(modelSpec, value) {
			t.Errorf("model value %q was rejected", value)
		}
	}
	if validateLabelValue(modelSpec, "some free text") {
		t.Error("free-text model value was accepted")
	}
}

func cloneCatalog() []MetricSpec {
	copied := make([]MetricSpec, len(catalog))
	copy(copied, catalog)
	for i := range copied {
		copied[i].Labels = append([]LabelSpec(nil), catalog[i].Labels...)
		copied[i].Buckets = append([]float64(nil), catalog[i].Buckets...)
	}
	return copied
}

func TestCatalogValidationRejectsInvalidSpecs(t *testing.T) {
	if err := validateCatalog(catalog); err != nil {
		t.Fatalf("frozen catalog is invalid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func([]MetricSpec) []MetricSpec
	}{
		{
			name: "duplicate name",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Name = specs[0].Name
				return specs
			},
		},
		{
			name: "forbidden label",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Labels = []LabelSpec{{
					Name: "case_id", Kind: LabelEnum, Allowed: []string{unknownLabelValue},
				}}
				return specs
			},
		},
		{
			name: "unknown label",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Labels = []LabelSpec{{
					Name: "mystery", Kind: LabelEnum, Allowed: []string{unknownLabelValue},
				}}
				return specs
			},
		},
		{
			name: "invalid type",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Type = "summary"
				return specs
			},
		},
		{
			name: "counter suffix",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[0].Type = TypeCounter
				return specs
			},
		},
		{
			name: "gauge suffix",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Type = TypeGauge
				return specs
			},
		},
		{
			name: "duplicate label",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Labels = []LabelSpec{
					{Name: "source", Kind: LabelSource},
					{Name: "source", Kind: LabelSource},
				}
				return specs
			},
		},
		{
			name: "enum without unknown",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Labels = []LabelSpec{{
					Name: "rule", Kind: LabelEnum, Allowed: []string{"exact"},
				}}
				return specs
			},
		},
		{
			name: "open label with values",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Labels = []LabelSpec{{
					Name: "source", Kind: LabelSource, Allowed: []string{"x"},
				}}
				return specs
			},
		},
		{
			name: "bad buckets",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[4].Buckets = []float64{300, 30}
				return specs
			},
		},
		{
			name: "zero cap",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].MaxSeries = 0
				return specs
			},
		},
		{
			name: "help punctuation",
			mutate: func(specs []MetricSpec) []MetricSpec {
				specs[1].Help = "missing period"
				return specs
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCatalog(test.mutate(cloneCatalog())); err == nil {
				t.Fatal("invalid catalog was accepted")
			}
		})
	}
}

func TestRegisterCheckedAcceptsCataloguedCollector(t *testing.T) {
	spec := mustSpec(t, MetricQueueDepth)
	collector := &staticCollector{
		desc:        prometheus.NewDesc(spec.Name, spec.Help, []string{"queue"}, nil),
		valueType:   prometheus.GaugeValue,
		value:       4,
		labelValues: []string{QueueCasesOpen},
	}
	registry := prometheus.NewRegistry()
	if err := RegisterChecked(registry, collector, spec); err != nil {
		t.Fatalf("RegisterChecked: %v", err)
	}
	views := gatherViews(t, registry)
	series := mustSeries(t, mustFamily(t, views, spec.Name), map[string]string{"queue": QueueCasesOpen})
	if got := gaugeValue(t, series); got != 4 {
		t.Errorf("checked gauge = %v, want 4", got)
	}
	if err := RegisterChecked(registry, collector, spec); err == nil {
		t.Error("duplicate checked registration succeeded")
	}
}

func TestRegisterCheckedRejectsBadCollectors(t *testing.T) {
	queueSpec := mustSpec(t, MetricQueueDepth)
	tests := []struct {
		name      string
		collector prometheus.Collector
		spec      MetricSpec
	}{
		{
			name: "forbidden label",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"case_id"}, nil),
				valueType:   prometheus.GaugeValue,
				value:       1,
				labelValues: []string{"abc"},
			},
			spec: queueSpec,
		},
		{
			name: "proposal id label",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"proposal_id"}, nil),
				valueType:   prometheus.GaugeValue,
				value:       1,
				labelValues: []string{"abc"},
			},
			spec: queueSpec,
		},
		{
			name: "trace id label",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"trace_id"}, nil),
				valueType:   prometheus.GaugeValue,
				value:       1,
				labelValues: []string{"abc"},
			},
			spec: queueSpec,
		},
		{
			name: "mismatched label set",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"status"}, nil),
				valueType:   prometheus.GaugeValue,
				value:       1,
				labelValues: []string{"open"},
			},
			spec: queueSpec,
		},
		{
			name: "free text value",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(MetricMatchRunsTotal, mustSpec(t, MetricMatchRunsTotal).Help, []string{"source"}, nil),
				valueType:   prometheus.CounterValue,
				value:       1,
				labelValues: []string{"customer free text"},
			},
			spec: mustSpec(t, MetricMatchRunsTotal),
		},
		{
			name: "uuid value",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(MetricMatchRunsTotal, mustSpec(t, MetricMatchRunsTotal).Help, []string{"source"}, nil),
				valueType:   prometheus.CounterValue,
				value:       1,
				labelValues: []string{"123e4567-e89b-12d3-a456-426614174000"},
			},
			spec: mustSpec(t, MetricMatchRunsTotal),
		},
		{
			name: "unknown enum value",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(MetricMatchesTotal, mustSpec(t, MetricMatchesTotal).Help, []string{"source", "rule"}, nil),
				valueType:   prometheus.CounterValue,
				value:       1,
				labelValues: []string{"bank", "mystery_rule"},
			},
			spec: mustSpec(t, MetricMatchesTotal),
		},
		{
			name: "empty collector",
			collector: &emptyCollector{
				desc: prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"queue"}, nil),
			},
			spec: queueSpec,
		},
		{
			name: "spec mismatch",
			collector: &staticCollector{
				desc:        prometheus.NewDesc(queueSpec.Name, queueSpec.Help, []string{"queue"}, nil),
				valueType:   prometheus.GaugeValue,
				value:       1,
				labelValues: []string{QueueCasesOpen},
			},
			spec: func() MetricSpec {
				spec := queueSpec
				spec.MaxSeries = 99
				return spec
			}(),
		},
		{
			name: "unknown family",
			collector: &staticCollector{
				desc:        prometheus.NewDesc("recon_custom_total", "Custom.", []string{"source"}, nil),
				valueType:   prometheus.CounterValue,
				value:       1,
				labelValues: []string{unknownLabelValue},
			},
			spec: MetricSpec{
				Name: "recon_custom_total", Type: TypeCounter, Unit: "custom",
				Help: "Custom.", Labels: []LabelSpec{sourceLabel}, MaxSeries: 1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := RegisterChecked(prometheus.NewRegistry(), test.collector, test.spec); err == nil {
				t.Fatal("bad collector was accepted")
			}
		})
	}
}

func TestValidateRegistryRejectsUnknownFamilies(t *testing.T) {
	metrics := mustNew(t, Options{Component: "api"})
	custom := &staticCollector{
		desc:        prometheus.NewDesc("recon_custom_total", "Custom.", []string{"source"}, nil),
		valueType:   prometheus.CounterValue,
		value:       1,
		labelValues: []string{unknownLabelValue},
	}
	if err := metrics.Registry().Register(custom); err != nil {
		t.Fatalf("register custom: %v", err)
	}
	err := metrics.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown recon metric family") {
		t.Fatalf("Validate = %v, want an unknown-family error", err)
	}
}

func TestValidateRegistryRejectsNonReconFamilies(t *testing.T) {
	metrics := mustNew(t, Options{Component: "api"})
	custom := &staticCollector{
		desc:        prometheus.NewDesc("custom_total", "Custom.", nil, nil),
		valueType:   prometheus.CounterValue,
		value:       1,
		labelValues: nil,
	}
	if err := metrics.Registry().Register(custom); err != nil {
		t.Fatalf("register custom: %v", err)
	}
	err := metrics.Validate()
	if err == nil || !strings.Contains(err.Error(), "non-recon metric family") {
		t.Fatalf("Validate = %v, want a non-recon-family error", err)
	}
}

func TestValidateRegistryExemptsClientFamilies(t *testing.T) {
	metrics := mustNew(t, Options{Component: "api", GoCollector: true})
	for _, name := range []string{"process_custom_seconds", "promhttp_custom_total"} {
		collector := &staticCollector{
			desc:        prometheus.NewDesc(name, "Exempt.", []string{"case_id"}, nil),
			valueType:   prometheus.GaugeValue,
			value:       1,
			labelValues: []string{"identifier-value"},
		}
		if err := metrics.Registry().Register(collector); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate with exempt families: %v", err)
	}
}
