package metrics

import (
	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/integrity"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
	"github.com/TamasGorgics/recon-agent/internal/worker"
)

// The adapters in this file are the only bridge between a domain package's
// narrow observer interface and the registry. Domain packages never import
// internal/metrics, which keeps the dependency direction one-way and avoids
// cycles. Every adapter is nil-safe so callers can construct one even when the
// registry is disabled.
var (
	_ match.Observer     = (*MatchObserver)(nil)
	_ cases.Observer     = (*CasesObserver)(nil)
	_ agent.RunObserver  = (*AgentObserver)(nil)
	_ worker.Observer    = (*WorkerObserver)(nil)
	_ approval.Observer  = (*ApprovalObserver)(nil)
	_ limits.Observer    = (*LimitsObserver)(nil)
	_ tieout.Observer    = (*TieoutObserver)(nil)
	_ integrity.Observer = (*IntegrityObserver)(nil)
)

// MatchObserver adapts the matcher's post-commit observer to the registry.
type MatchObserver struct {
	metrics *Metrics
}

// NewMatchObserver returns an adapter for one process registry.
func NewMatchObserver(m *Metrics) *MatchObserver { return &MatchObserver{metrics: m} }

// MatchRun records one committed run and its accepted matches/breaks.
func (o *MatchObserver) MatchRun(source string, stats match.RunStats) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveMatchRun(source, stats.ByPass, stats.BreaksBySide)
}

// MatchFailed records one bounded matcher failure code.
func (o *MatchObserver) MatchFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("matcher", code)
}

// CasesObserver adapts the case service's observer to the registry. Component
// is the process that owns the service (api or worker); the frozen error
// component allow-list has no separate cases value.
type CasesObserver struct {
	metrics   *Metrics
	component string
}

// NewCasesObserver returns an adapter for one process registry and the
// component that runs the case service.
func NewCasesObserver(m *Metrics, component string) *CasesObserver {
	return &CasesObserver{metrics: m, component: component}
}

// CaseAge records one committed case status and its age in seconds.
func (o *CasesObserver) CaseAge(status string, ageSeconds float64) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveCaseAge(status, ageSeconds)
}

// CaseFailed records one bounded case-service failure code under the owning
// process component.
func (o *CasesObserver) CaseFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError(o.component, code)
}

// AgentObserver adapts the agent run store's observer to the registry.
type AgentObserver struct {
	metrics *Metrics
}

// NewAgentObserver returns an adapter for one process registry.
func NewAgentObserver(m *Metrics) *AgentObserver { return &AgentObserver{metrics: m} }

// AgentRun records one committed agent run outcome and its cost.
func (o *AgentObserver) AgentRun(outcome, model string, costUSD float64) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveAgentRun(outcome, model, costUSD)
}

// AgentFailed records one bounded agent-store failure code.
func (o *AgentObserver) AgentFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("agent", code)
}

// WorkerObserver adapts the worker's failure observer to the registry.
type WorkerObserver struct {
	metrics *Metrics
}

// NewWorkerObserver returns an adapter for one process registry.
func NewWorkerObserver(m *Metrics) *WorkerObserver { return &WorkerObserver{metrics: m} }

// WorkerFailed records one bounded worker failure code.
func (o *WorkerObserver) WorkerFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("worker", code)
}

// ApprovalObserver adapts the approval service's post-commit decision
// observer to the registry.
type ApprovalObserver struct {
	metrics *Metrics
}

// NewApprovalObserver returns an adapter for one process registry.
func NewApprovalObserver(m *Metrics) *ApprovalObserver { return &ApprovalObserver{metrics: m} }

// ApprovalDecided records one committed, non-replayed human decision latency.
func (o *ApprovalObserver) ApprovalDecided(tier string, latencySeconds float64) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveApprovalDecision(tier, latencySeconds)
}

// ApprovalFailed records one bounded approval failure code.
func (o *ApprovalObserver) ApprovalFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("approval", code)
}

// LimitsObserver adapts the limits service's post-check observer to the
// registry.
type LimitsObserver struct {
	metrics *Metrics
}

// NewLimitsObserver returns an adapter for one process registry.
func NewLimitsObserver(m *Metrics) *LimitsObserver { return &LimitsObserver{metrics: m} }

// CapUsageRatio records the clamped daily auto-apply cap usage for a currency.
func (o *LimitsObserver) CapUsageRatio(currency string, ratio float64) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.SetAutoApplyCapUsageRatio(currency, ratio)
}

// CheckFailed records one bounded limits failure code.
func (o *LimitsObserver) CheckFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("limits", code)
}

// IntegrityObserver adapts the integrity job's post-commit observer to the
// single frozen integrity gauge. It never sets 0: the seeded 0 means
// unknown/not run, and the adapter is only called after a completed run.
type IntegrityObserver struct {
	metrics *Metrics
}

// NewIntegrityObserver returns an adapter for one process registry.
func NewIntegrityObserver(m *Metrics) *IntegrityObserver {
	return &IntegrityObserver{metrics: m}
}

// IntegrityRunCompleted sets the integrity job gauge to 1 for a passed run and
// 2 for a failed run. It is called exactly once per committed run.
func (o *IntegrityObserver) IntegrityRunCompleted(passed bool) {
	if o == nil || o.metrics == nil {
		return
	}
	if passed {
		o.metrics.SetIntegrityJobStatus(JobIntegrity, 1)
		return
	}
	o.metrics.SetIntegrityJobStatus(JobIntegrity, 2)
}

// TieoutObserver adapts a tie-out computation to the registry.
type TieoutObserver struct {
	metrics *Metrics
}

// NewTieoutObserver returns an adapter for one process registry.
func NewTieoutObserver(m *Metrics) *TieoutObserver { return &TieoutObserver{metrics: m} }

// TieoutComputed sets the signed unexplained amount for the scope's currency
// and source. The result source is the scope's source, never an identifier.
func (o *TieoutObserver) TieoutComputed(result tieout.Result) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.SetTieoutUnexplainedMinor(result.Currency, result.Source, result.Unexplained.AmountMinor())
}

// TieoutFailed records one bounded tie-out failure code.
func (o *TieoutObserver) TieoutFailed(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveError("tieout", code)
}
