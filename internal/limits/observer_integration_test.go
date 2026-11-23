//go:build integration

package limits_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

type recordedCapUsage struct {
	currency string
	ratio    float64 //floatlint:allow the Prometheus gauge boundary is float64 ratio
}

type recordingLimitsObserver struct {
	mu       sync.Mutex
	ratios   []recordedCapUsage
	failures []string
}

func (o *recordingLimitsObserver) CapUsageRatio(currency string, ratio float64) { //floatlint:allow the Prometheus gauge boundary is float64 ratio
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ratios = append(o.ratios, recordedCapUsage{currency: currency, ratio: ratio})
}

func (o *recordingLimitsObserver) CheckFailed(code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, code)
}

func (o *recordingLimitsObserver) snapshot() ([]recordedCapUsage, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]recordedCapUsage(nil), o.ratios...), append([]string(nil), o.failures...)
}

func (f *fixture) serviceWithObserver(t *testing.T, observer limits.Observer) *limits.Service {
	t.Helper()
	svc, err := limits.NewService(limits.Options{
		Pool:     f.pool,
		Clock:    f.clock,
		Config:   config.DefaultAutoApplyLimitsConfig(),
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("limits.NewService with observer: %v", err)
	}
	return svc
}

// TestCheckObserverRecordsClampedRatio proves a 60% cumulative use sets the
// clamped ratio for the candidate currency.
func TestCheckObserverRecordsClampedRatio(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	observer := &recordingLimitsObserver{}
	svc := f.serviceWithObserver(t, observer)

	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(6_000, "USD", "debit")),
		createdAt: testNow.Add(-time.Hour),
	})

	result, err := svc.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 1_000,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed || result.Reason != limits.ReasonWithinLimits {
		t.Fatalf("Check = %+v, want an allowed within_limits result", result)
	}
	ratios, failures := observer.snapshot()
	if len(failures) != 0 {
		t.Fatalf("allowed check emitted failures: %v", failures)
	}
	if len(ratios) != 1 {
		t.Fatalf("allowed check emitted %d ratios, want 1: %+v", len(ratios), ratios)
	}
	if ratios[0].currency != "USD" || ratios[0].ratio != 0.6 {
		t.Fatalf("ratio = %+v, want USD 0.6", ratios[0])
	}
}

// TestCheckObserverZeroCapEmitsNoRatio proves a currency with no configured
// positive cap fails closed without inventing a gauge value.
func TestCheckObserverZeroCapEmitsNoRatio(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	observer := &recordingLimitsObserver{}
	svc := f.serviceWithObserver(t, observer)

	result, err := svc.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "GBP",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Reason != limits.ReasonCurrencyCapMissing || result.CapMinor != 0 {
		t.Fatalf("Check = %+v, want currency_cap_missing with zero cap", result)
	}
	ratios, failures := observer.snapshot()
	if len(ratios) != 0 {
		t.Fatalf("zero-cap check emitted ratios: %+v", ratios)
	}
	if len(failures) != 0 {
		t.Fatalf("zero-cap check emitted failures: %s", failures)
	}
}

// TestCheckObserverRecordsDailyCap proves a cap denial sets the ratio and the
// bounded daily_cap_exceeded error code.
func TestCheckObserverRecordsDailyCap(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	observer := &recordingLimitsObserver{}
	svc := f.serviceWithObserver(t, observer)

	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(9_500, "USD", "debit")),
		createdAt: testNow.Add(-time.Hour),
	})

	result, err := svc.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 1_000,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("Check = %+v, want daily_cap_exceeded", result)
	}
	ratios, failures := observer.snapshot()
	if len(ratios) != 1 || ratios[0].currency != "USD" || ratios[0].ratio != 0.95 {
		t.Fatalf("ratios = %+v, want one USD 0.95", ratios)
	}
	if len(failures) != 1 || failures[0] != limits.ErrorCodeDailyCapExceeded {
		t.Fatalf("failures = %v, want [daily_cap_exceeded]", failures)
	}
}
