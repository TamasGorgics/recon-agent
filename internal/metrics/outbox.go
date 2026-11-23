package metrics

import "github.com/TamasGorgics/recon-agent/internal/outbox"

// OutboxMetrics adapts the outbox worker's delivery-outcome seam to the
// registry. Every method is called by the worker only after the corresponding
// database mark commits, so a counter never runs ahead of the authoritative
// row state. The adapter is nil-safe: a process without a registry can still
// construct it.
var _ outbox.Metrics = (*OutboxMetrics)(nil)

// OutboxMetrics implements outbox.Metrics for one process registry.
type OutboxMetrics struct {
	metrics *Metrics
}

// NewOutboxMetrics returns an adapter for one process registry. A nil registry
// makes every method a no-op.
func NewOutboxMetrics(m *Metrics) *OutboxMetrics { return &OutboxMetrics{metrics: m} }

// Delivered is intentionally a no-op: successful delivery is already visible
// through the outbox row's delivered_at and has no frozen success counter.
func (o *OutboxMetrics) Delivered(string) {}

// Failed records one bounded delivery failure. The errors family has no topic
// label, so the topic is deliberately not forwarded.
func (o *OutboxMetrics) Failed(string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveOutboxFailure()
}

// DeadLettered records one dead-lettered message under the normalized topic;
// a topic outside the frozen allow-list folds to unknown. Attempts is not a
// label.
func (o *OutboxMetrics) DeadLettered(topic string, _ int) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.ObserveOutboxDeadLetter(topic)
}
