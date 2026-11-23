package metrics

import (
	"math"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// TestApprovalObserverRecordsLatencyAndFailure proves the approval adapter
// maps a tier to the histogram, folds an unrecognized tier to unknown, and
// records only bounded failure codes.
func TestApprovalObserverRecordsLatencyAndFailure(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	observer := NewApprovalObserver(m)
	observer.ApprovalDecided("T1", 90)
	observer.ApprovalDecided("T9", 12)
	observer.ApprovalFailed("conflict")
	observer.ApprovalFailed("free text: proposal 123e4567-e89b-12d3-a456-426614174000")

	views := gatherViews(t, m.Registry())
	family := mustFamily(t, views, MetricApprovalLatencySeconds)
	for tier, want := range map[string]float64{"T1": 90, unknownLabelValue: 12} {
		histogram := histogramValue(t, mustSeries(t, family, map[string]string{"tier": tier}))
		if histogram.count != 1 || math.Abs(histogram.sum-want) > 1e-9 {
			t.Fatalf("approval latency{tier=%s} = count %d sum %v, want 1/%v",
				tier, histogram.count, histogram.sum, want)
		}
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	for code, want := range map[string]float64{"conflict": 1, unknownLabelValue: 1} {
		series := mustSeries(t, errorsFamily, map[string]string{"component": "approval", "code": code})
		if got := counterValue(t, series); got != want {
			t.Fatalf("approval errors{code=%s} = %v, want %v", code, got, want)
		}
	}
}

// TestLimitsObserverRecordsRatioAndFailure proves the limits adapter sets the
// clamped ratio, folds an invalid currency, and records bounded failures.
func TestLimitsObserverRecordsRatioAndFailure(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	observer := NewLimitsObserver(m)
	observer.CapUsageRatio("USD", 0.6)
	observer.CapUsageRatio("usd", 0.25)
	observer.CapUsageRatio("EUR", 1.7)
	observer.CheckFailed("daily_cap_exceeded")
	observer.CheckFailed("raw failure text")

	views := gatherViews(t, m.Registry())
	capFamily := mustFamily(t, views, MetricAutoApplyCapUsageRatio)
	for currency, want := range map[string]float64{"USD": 0.6, unknownLabelValue: 0.25, "EUR": 1} {
		series := mustSeries(t, capFamily, map[string]string{"currency": currency})
		if got := gaugeValue(t, series); math.Abs(got-want) > 1e-9 {
			t.Fatalf("cap ratio{currency=%s} = %v, want %v", currency, got, want)
		}
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	for code, want := range map[string]float64{"daily_cap_exceeded": 1, unknownLabelValue: 1} {
		series := mustSeries(t, errorsFamily, map[string]string{"component": "limits", "code": code})
		if got := counterValue(t, series); got != want {
			t.Fatalf("limits errors{code=%s} = %v, want %v", code, got, want)
		}
	}
}

// TestTieoutObserverRecordsUnexplained proves the tie-out adapter sets the
// signed gauge from the result's currency/source and never uses the scope id.
func TestTieoutObserverRecordsUnexplained(t *testing.T) {
	m := mustNew(t, Options{Component: "api"})
	observer := NewTieoutObserver(m)
	usd := money.MustParseCurrency("USD")
	observer.TieoutComputed(tieout.Result{
		ScopeID:     "123e4567-e89b-12d3-a456-426614174000",
		Source:      "psp_a",
		Currency:    "USD",
		Unexplained: money.FromMinor(-123, usd),
	})
	observer.TieoutComputed(tieout.Result{
		Source:      "123e4567-e89b-12d3-a456-426614174000",
		Currency:    "usd",
		Unexplained: money.FromMinor(7, usd),
	})
	observer.TieoutFailed("not_found")

	views := gatherViews(t, m.Registry())
	family := mustFamily(t, views, MetricTieoutUnexplainedMinor)
	series := mustSeries(t, family, map[string]string{"currency": "USD", "source": "psp_a"})
	if got := gaugeValue(t, series); got != -123 {
		t.Fatalf("tie-out psp_a = %v, want -123", got)
	}
	folded := mustSeries(t, family, map[string]string{"currency": unknownLabelValue, "source": unknownLabelValue})
	if got := gaugeValue(t, folded); got != 7 {
		t.Fatalf("tie-out unknown = %v, want 7", got)
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	if got := counterValue(t, mustSeries(t, errorsFamily,
		map[string]string{"component": "tieout", "code": "not_found"})); got != 1 {
		t.Fatalf("tie-out errors = %v, want 1", got)
	}
}

// TestOutboxMetricsAdapter proves the outbox adapter is a no-op for delivery,
// counts bounded failures, and folds an unknown topic to unknown for dead
// letters and dead-lettered attempts are not labels.
func TestOutboxMetricsAdapter(t *testing.T) {
	m := mustNew(t, Options{Component: "outbox"})
	adapter := NewOutboxMetrics(m)
	adapter.Delivered("approval.request")
	adapter.Failed("approval.request")
	adapter.Failed("approval.cancel")
	adapter.DeadLettered("approval.request", 5)
	adapter.DeadLettered("approval.update", 1)
	adapter.DeadLettered("approval.cancel", 0)
	adapter.DeadLettered("unexpected.topic", 3)

	views := gatherViews(t, m.Registry())
	dead := mustFamily(t, views, MetricOutboxDeadLettersTotal)
	for topic, want := range map[string]float64{
		"approval.request": 1,
		"approval.update":  1,
		"approval.cancel":  1,
		unknownLabelValue:  1,
	} {
		if got := counterValue(t, mustSeries(t, dead, map[string]string{"topic": topic})); got != want {
			t.Fatalf("dead letters{topic=%s} = %v, want %v", topic, got, want)
		}
	}
	errorsFamily := mustFamily(t, views, MetricErrorsTotal)
	if got := counterValue(t, mustSeries(t, errorsFamily,
		map[string]string{"component": "outbox", "code": "delivery_failed"})); got != 2 {
		t.Fatalf("outbox delivery failures = %v, want 2", got)
	}
	if got := counterValue(t, mustSeries(t, dead, map[string]string{"topic": unknownLabelValue})); got != 1 {
		t.Fatalf("unknown dead letters = %v, want 1", got)
	}
}

// TestW3BAdaptersAreNilSafe proves a process without a registry cannot panic
// and the outbox adapter still satisfies the worker seam.
func TestW3BAdaptersAreNilSafe(t *testing.T) {
	var (
		approvalObserver *ApprovalObserver
		limitsObserver   *LimitsObserver
		tieoutObserver   *TieoutObserver
		outboxAdapter    = NewOutboxMetrics(nil)
	)
	approvalObserver.ApprovalDecided("T1", 1)
	approvalObserver.ApprovalFailed("internal")
	limitsObserver.CapUsageRatio("USD", 0.5)
	limitsObserver.CheckFailed("internal")
	tieoutObserver.TieoutComputed(tieout.Result{})
	tieoutObserver.TieoutFailed("internal")
	outboxAdapter.Delivered("approval.request")
	outboxAdapter.Failed("approval.request")
	outboxAdapter.DeadLettered("approval.request", 1)
}
