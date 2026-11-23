package metrics

// Frozen metric family names. These are a public contract; renaming one
// requires a specification version note.
const (
	MetricBuildInfo              = "recon_build_info"
	MetricMatchRunsTotal         = "recon_match_runs_total"
	MetricMatchesTotal           = "recon_matches_total"
	MetricBreaksTotal            = "recon_breaks_total"
	MetricCaseAgeSeconds         = "recon_case_age_seconds"
	MetricQueueDepth             = "recon_queue_depth"
	MetricAgentRunsTotal         = "recon_agent_runs_total"
	MetricAgentCostUSDTotal      = "recon_agent_cost_usd_total"
	MetricApprovalLatencySeconds = "recon_approval_latency_seconds"
	MetricErrorsTotal            = "recon_errors_total"
	MetricOutboxLagSeconds       = "recon_outbox_lag_seconds"
	MetricOutboxDeadLettersTotal = "recon_outbox_dead_letters_total"
	MetricAutoApplyCapUsageRatio = "recon_auto_apply_cap_usage_ratio"
	MetricTieoutUnexplainedMinor = "recon_tieout_unexplained_minor"
	MetricIntegrityJobStatus     = "recon_integrity_job_status"
)

// Queue and job label values that intentionally have no unknown child. They
// are closed internal sets produced by code, not user input.
const (
	QueueCasesOpen        = "cases_open"
	QueueApprovalsPending = "approvals_pending"
	QueueOutboxPending    = "outbox_pending"
	JobIntegrity          = "integrity"
)

// Type is the Prometheus metric type in the frozen catalog.
type Type string

const (
	TypeCounter   Type = "counter"
	TypeGauge     Type = "gauge"
	TypeHistogram Type = "histogram"
)

// LabelKind selects the validator for a label value.
type LabelKind string

const (
	// LabelEnum is a closed set in Allowed.
	LabelEnum LabelKind = "enum"
	// LabelSource uses ^[a-z0-9][a-z0-9_]{0,31}$ with unknown fallback.
	LabelSource LabelKind = "source"
	// LabelCurrency uses ^[A-Z]{3}$ with unknown fallback.
	LabelCurrency LabelKind = "currency"
	// LabelModel uses ^[A-Za-z0-9._:/-]{1,64}$ with unknown fallback.
	LabelModel LabelKind = "model"
)

// LabelSpec declares one label in a metric family.
type LabelSpec struct {
	Name    string
	Kind    LabelKind
	Allowed []string
}

// MetricSpec declares one metric family: its frozen name, type, unit, help,
// labels, histogram buckets, and cardinality cap. Labels are declared in the
// specification-table order; Prometheus sorts label pairs in gathered output.
type MetricSpec struct {
	Name      string
	Type      Type
	Unit      string
	Help      string
	Labels    []LabelSpec
	Buckets   []float64
	MaxSeries int
}

var (
	sourceLabel   = LabelSpec{Name: "source", Kind: LabelSource}
	currencyLabel = LabelSpec{Name: "currency", Kind: LabelCurrency}
	modelLabel    = LabelSpec{Name: "model", Kind: LabelModel}

	ruleLabel   = LabelSpec{Name: "rule", Kind: LabelEnum, Allowed: ruleValues}
	sideLabel   = LabelSpec{Name: "side", Kind: LabelEnum, Allowed: sideValues}
	statusLabel = LabelSpec{
		Name: "status", Kind: LabelEnum, Allowed: statusValues,
	}
	outcomeLabel = LabelSpec{
		Name: "outcome", Kind: LabelEnum, Allowed: outcomeValues,
	}
	tierLabel  = LabelSpec{Name: "tier", Kind: LabelEnum, Allowed: tierValues}
	queueLabel = LabelSpec{
		Name: "queue", Kind: LabelEnum, Allowed: queueValues,
	}
	topicLabel = LabelSpec{Name: "topic", Kind: LabelEnum, Allowed: topicValues}
	jobLabel   = LabelSpec{Name: "job", Kind: LabelEnum, Allowed: jobValues}
	// errorComponentLabel and buildComponentLabel share a name but have
	// different allow-lists: errors can be emitted by unknown, while build
	// info is always one of the three long-running binaries.
	errorComponentLabel = LabelSpec{
		Name: "component", Kind: LabelEnum, Allowed: componentValues,
	}
	buildComponentLabel = LabelSpec{
		Name: "component", Kind: LabelEnum, Allowed: buildComponentValues,
	}
	codeLabel = LabelSpec{Name: "code", Kind: LabelEnum, Allowed: codeValues}
)

var (
	ruleValues = []string{
		"exact", "reference_tolerance", "composite_batch", "composite",
		"fuzzy", "duplicate", unknownLabelValue,
	}
	sideValues = []string{
		"ledger_only", "external_only", "amount_mismatch", "duplicate",
		unknownLabelValue,
	}
	statusValues = []string{
		"open", "in_agent", "proposal_pending", "waiting_info", "escalated",
		"resolved", unknownLabelValue,
	}
	outcomeValues = []string{
		"proposal", "escalated", "budget_exhausted", "invalid_output",
		"provider_error", unknownLabelValue,
	}
	tierValues  = []string{"T0", "T1", "T2", unknownLabelValue}
	queueValues = []string{QueueCasesOpen, QueueApprovalsPending, QueueOutboxPending}
	topicValues = []string{
		"approval.request", "approval.update", "approval.cancel",
		unknownLabelValue,
	}
	jobValues            = []string{JobIntegrity, unknownLabelValue}
	buildComponentValues = []string{"api", "worker", "outbox"}
	componentValues      = []string{
		"api", "worker", "agent", "matcher", "approval", "limits", "outbox",
		"tieout", "metrics", "integrity", unknownLabelValue,
	}
	codeValues = []string{
		"internal", "invalid_request", "not_found", "conflict", "unauthorized",
		"forbidden", "timeout", "unavailable", "provider_error",
		"invalid_output", "delivery_failed", "dead_letter", "collect",
		"daily_cap_exceeded", unknownLabelValue,
	}
)

// Frozen help text. It is asserted against gathered output by the registry
// test and must not be reworded without a contract update.
const (
	helpBuildInfo  = "Build information for this process; the value is always 1."
	helpMatchRuns  = "Total number of matcher runs that committed successfully."
	helpMatches    = "Total number of accepted matches produced by matcher runs, by rule."
	helpBreaks     = "Total number of reconciliation breaks detected, by side."
	helpCaseAge    = "Age in seconds of a reconciliation case when its status was observed."
	helpQueue      = "Number of items currently waiting in a queue."
	helpAgentRuns  = "Total number of agent runs that finished, by outcome and model."
	helpAgentCost  = "Total agent cost in US dollars, by model."
	helpApproval   = "Latency in seconds from proposal creation to a human approval decision, by risk tier."
	helpErrors     = "Total number of bounded errors, by component and code."
	helpOutboxLag  = "Age in seconds of the oldest undelivered outbox message, by topic."
	helpDeadLetter = "Total number of outbox messages that reached the dead-letter state, by topic."
	helpCapRatio   = "Ratio of the daily auto-apply cap currently in use, clamped to the range 0 to 1."
	helpTieout     = "Signed unexplained amount in minor currency units from the latest tie-out computation."
	helpIntegrity  = "Integrity job status: 0=unknown/not run, 1=pass, 2=fail."
)

// caseAgeBuckets is the frozen spec §4.2 boundary set.
var caseAgeBuckets = []float64{
	30, 300, 900, 3600, 21600, 43200, 86400, 172800, 259200, 604800, 2592000,
}

// approvalLatencyBuckets is the frozen spec §4.2 boundary set.
var approvalLatencyBuckets = []float64{
	30, 60, 300, 900, 1800, 3600, 14400, 86400, 259200,
}

// catalog is the single source of truth for every recon_* family. The order
// follows the specification table so the registry test reads as the contract.
var catalog = []MetricSpec{
	{
		Name: MetricBuildInfo, Type: TypeGauge, Unit: "info", Help: helpBuildInfo,
		Labels: []LabelSpec{buildComponentLabel}, MaxSeries: 3,
	},
	{
		Name: MetricMatchRunsTotal, Type: TypeCounter, Unit: "runs", Help: helpMatchRuns,
		Labels: []LabelSpec{sourceLabel}, MaxSeries: 32,
	},
	{
		Name: MetricMatchesTotal, Type: TypeCounter, Unit: "matches", Help: helpMatches,
		Labels: []LabelSpec{sourceLabel, ruleLabel}, MaxSeries: 64,
	},
	{
		Name: MetricBreaksTotal, Type: TypeCounter, Unit: "breaks", Help: helpBreaks,
		Labels: []LabelSpec{sourceLabel, sideLabel}, MaxSeries: 32,
	},
	{
		Name: MetricCaseAgeSeconds, Type: TypeHistogram, Unit: "seconds",
		Help: helpCaseAge, Labels: []LabelSpec{statusLabel},
		Buckets: caseAgeBuckets, MaxSeries: 8,
	},
	{
		Name: MetricQueueDepth, Type: TypeGauge, Unit: "items", Help: helpQueue,
		Labels: []LabelSpec{queueLabel}, MaxSeries: 3,
	},
	{
		Name: MetricAgentRunsTotal, Type: TypeCounter, Unit: "runs", Help: helpAgentRuns,
		Labels: []LabelSpec{outcomeLabel, modelLabel}, MaxSeries: 64,
	},
	{
		Name: MetricAgentCostUSDTotal, Type: TypeCounter, Unit: "USD",
		Help: helpAgentCost, Labels: []LabelSpec{modelLabel}, MaxSeries: 32,
	},
	{
		Name: MetricApprovalLatencySeconds, Type: TypeHistogram, Unit: "seconds",
		Help: helpApproval, Labels: []LabelSpec{tierLabel},
		Buckets: approvalLatencyBuckets, MaxSeries: 4,
	},
	{
		Name: MetricErrorsTotal, Type: TypeCounter, Unit: "errors", Help: helpErrors,
		Labels: []LabelSpec{errorComponentLabel, codeLabel}, MaxSeries: 64,
	},
	{
		Name: MetricOutboxLagSeconds, Type: TypeGauge, Unit: "seconds",
		Help: helpOutboxLag, Labels: []LabelSpec{topicLabel}, MaxSeries: 4,
	},
	{
		Name: MetricOutboxDeadLettersTotal, Type: TypeCounter, Unit: "messages",
		Help: helpDeadLetter, Labels: []LabelSpec{topicLabel}, MaxSeries: 4,
	},
	{
		Name: MetricAutoApplyCapUsageRatio, Type: TypeGauge, Unit: "ratio 0-1",
		Help: helpCapRatio, Labels: []LabelSpec{currencyLabel}, MaxSeries: 32,
	},
	{
		Name: MetricTieoutUnexplainedMinor, Type: TypeGauge, Unit: "minor units",
		Help: helpTieout, Labels: []LabelSpec{currencyLabel, sourceLabel},
		MaxSeries: 64,
	},
	{
		Name: MetricIntegrityJobStatus, Type: TypeGauge, Unit: "status",
		Help: helpIntegrity, Labels: []LabelSpec{jobLabel}, MaxSeries: 2,
	},
}

// catalogSpec returns the frozen specification for a metric family.
func catalogSpec(name string) (MetricSpec, bool) {
	for _, spec := range catalog {
		if spec.Name == name {
			return spec, true
		}
	}
	return MetricSpec{}, false
}

func (s MetricSpec) labelNames() []string {
	names := make([]string, len(s.Labels))
	for i, label := range s.Labels {
		names[i] = label.Name
	}
	return names
}
