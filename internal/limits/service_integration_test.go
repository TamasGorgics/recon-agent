//go:build integration

// covers: FIN-53
package limits_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

func TestCheckPassthroughAndInvalidValues(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, tier := range []policy.Tier{policy.TierT1, policy.TierT2} {
		result, err := f.service.Check(ctx, limits.Candidate{
			ProposalID: "passthrough-" + string(tier),
			Currency:   "USD",
			ValueMinor: 100,
			Tier:       tier,
		})
		if err != nil {
			t.Fatalf("Check(%s): %v", tier, err)
		}
		if !result.Allowed || result.Tier != tier || result.Reason != limits.ReasonNotT0 {
			t.Fatalf("Check(%s) = %+v, want an allowed passthrough", tier, result)
		}
	}

	// A T1 candidate passes through even when its value would be invalid for
	// a T0 candidate; Check only gates T0.
	if result, err := f.service.Check(ctx, limits.Candidate{
		Currency: "USD", ValueMinor: -1, Tier: policy.TierT1,
	}); err != nil || !result.Allowed || result.Reason != limits.ReasonNotT0 {
		t.Fatalf("negative T1 = %+v, %v; want passthrough", result, err)
	}

	for _, candidate := range []limits.Candidate{
		{Currency: "USD", ValueMinor: -1, Tier: policy.TierT0},
		{Currency: "usd", ValueMinor: 100, Tier: policy.TierT0},
		{Currency: "US", ValueMinor: 100, Tier: policy.TierT0},
		{Currency: "", ValueMinor: 100, Tier: policy.TierT0},
	} {
		result, err := f.service.Check(ctx, candidate)
		if err != nil {
			t.Fatalf("Check(%+v): %v", candidate, err)
		}
		if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonValueUnknown {
			t.Fatalf("Check(%+v) = %+v, want T1 value_unknown", candidate, result)
		}
	}

	// Invalid input is rejected before any switch read or trip.
	if row := f.readSwitch(t); row.Enabled {
		t.Fatalf("invalid candidates changed the switch: %+v", row)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("invalid candidates wrote %d switch audits", got)
	}
}

func TestCheckMalformedProposalIDFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	valid := "123e4567-e89b-42d3-a456-426614174000"
	for _, proposalID := range []string{
		"not-a-uuid",
		strings.ToUpper(valid),
		strings.ReplaceAll(valid, "-", ""),
		" " + valid,
	} {
		t.Run("proposal-id-"+proposalID, func(t *testing.T) {
			result, err := f.service.Check(context.Background(), limits.Candidate{
				ProposalID: proposalID,
				Currency:   "USD",
				ValueMinor: 100,
				Tier:       policy.TierT0,
			})
			if err != nil {
				t.Fatalf("Check(%q): %v", proposalID, err)
			}
			if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonLimitsUnavailable {
				t.Fatalf("Check(%q) = %+v, want T1 limits_unavailable", proposalID, result)
			}
			if result.BreakerTripped || result.SwitchEnabled || result.CumulativeMinor != 0 || result.CapMinor != 0 {
				t.Fatalf("Check(%q) = %+v, want a typed failed-closed result with no switch read", proposalID, result)
			}
		})
	}
	if row := f.readSwitch(t); !row.Enabled {
		t.Fatalf("malformed proposal IDs changed the switch: %+v", row)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("malformed proposal IDs wrote %d audits", got)
	}
}

func TestCheckSwitchOffFailsClosedWithoutTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if row := f.readSwitch(t); row.Enabled {
		t.Fatalf("fixture switch = %+v, want the seeded false value", row)
	}

	result, err := f.service.Check(ctx, limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonAutoApplyDisabled {
		t.Fatalf("Check = %+v, want T1 auto_apply_disabled", result)
	}
	if result.BreakerTripped || result.SwitchEnabled {
		t.Fatalf("Check = %+v, want no trip and a disabled switch", result)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("switch-off check wrote %d audits", got)
	}
	if row := f.readSwitch(t); row.Enabled || row.Reason != "seed" {
		t.Fatalf("switch-off check changed the switch: %+v", row)
	}
}

func TestCheckCumulativeAndDailyCapBoundaries(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	ctx := context.Background()
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	// Counted: USD T0 auto_approved today, 6,000 debit plus one credit line.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(6000, "USD", "debit"), lineJSON(500, "USD", "credit")),
		createdAt: dayStart.Add(2 * time.Hour),
	})
	// Counted: USD T0 applied today though created yesterday.
	appliedAt := dayStart.Add(3 * time.Hour)
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(3000, "USD", "debit")),
		status:    "applied",
		createdAt: dayStart.Add(-2 * time.Hour),
		appliedAt: &appliedAt,
	})
	// Ignored: created yesterday and never applied today.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(7000, "USD", "debit")),
		createdAt: dayStart.Add(-12 * time.Hour),
	})
	// Ignored: wrong status.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(9000, "USD", "debit")),
		status:    "proposed",
		createdAt: dayStart.Add(4 * time.Hour),
	})
	// Ignored: wrong risk tier.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(9000, "USD", "debit")),
		tier:      "T1",
		createdAt: dayStart.Add(4 * time.Hour),
	})
	// Ignored: wrong currency.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(8000, "EUR", "debit")),
		currency:  "EUR",
		createdAt: dayStart.Add(4 * time.Hour),
	})
	// Zero: link/duplicate payloads have no lines.
	f.seedProposal(t, proposalSpec{
		proposalType: "link",
		payload:      `{"duplicate_of":null}`,
		createdAt:    dayStart.Add(4 * time.Hour),
	})
	f.seedProposal(t, proposalSpec{
		proposalType: "duplicate",
		payload:      `{}`,
		createdAt:    dayStart.Add(4 * time.Hour),
	})

	for _, test := range []struct {
		name       string
		value      int64
		wantTotal  int64
		wantTrip   bool
		wantReason string
	}{
		{"below cap", 999, 9000, false, limits.ReasonWithinLimits},
		{"exactly at cap", 1000, 9000, false, limits.ReasonWithinLimits},
		{"above cap", 1001, 9000, true, limits.ReasonDailyCapExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := f.service.Check(ctx, limits.Candidate{
				ProposalID: testCandidateID,
				Currency:   "USD",
				ValueMinor: test.value,
				Tier:       policy.TierT0,
			})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if result.CumulativeMinor != test.wantTotal {
				t.Fatalf("CumulativeMinor = %d, want %d (result %+v)", result.CumulativeMinor, test.wantTotal, result)
			}
			if result.CapMinor != 10000 {
				t.Fatalf("CapMinor = %d, want 10000", result.CapMinor)
			}
			if test.wantTrip {
				if result.Allowed || result.Tier != policy.TierT1 || result.Reason != test.wantReason {
					t.Fatalf("Check = %+v, want T1 %s", result, test.wantReason)
				}
				if !result.BreakerTripped || result.SwitchEnabled {
					t.Fatalf("Check = %+v, want a trip and disabled switch", result)
				}
				if row := f.readSwitch(t); row.Enabled {
					t.Fatalf("switch stayed enabled after a cap trip: %+v", row)
				}
				return
			}
			if !result.Allowed || result.Tier != policy.TierT0 || result.Reason != test.wantReason {
				t.Fatalf("Check = %+v, want allowed T0 %s", result, test.wantReason)
			}
			if result.BreakerTripped || !result.SwitchEnabled {
				t.Fatalf("Check = %+v, want no trip and an enabled switch", result)
			}
		})
	}
}

func TestCheckExcludesCandidateProposalFromCumulative(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	ctx := context.Background()
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	// Other T0 rows already count 5,000 against the 10,000 USD cap.
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(5000, "USD", "debit")),
		createdAt: dayStart.Add(time.Hour),
	})
	// The candidate's own row is already persisted as auto_approved for 3,000,
	// so the raw cumulative is 8,000. Without the exclusion, adding ValueMinor
	// again (11,000) would trip prematurely.
	candidateID := f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(3000, "USD", "debit")),
		createdAt: dayStart.Add(2 * time.Hour),
	})
	candidate := limits.Candidate{
		ProposalID: candidateID,
		Currency:   "USD",
		ValueMinor: 3000,
		Tier:       policy.TierT0,
	}

	result, err := f.service.Check(ctx, candidate)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !result.Allowed || result.Tier != policy.TierT0 || result.Reason != limits.ReasonWithinLimits {
		t.Fatalf("Check = %+v, want allowed T0 within_limits", result)
	}
	if result.CumulativeMinor != 5000 {
		t.Fatalf("CumulativeMinor = %d, want 5000 after excluding the candidate row", result.CumulativeMinor)
	}
	if got := result.CumulativeMinor + candidate.ValueMinor; got != 8000 {
		t.Fatalf("true total = %d, want 8000", got)
	}
	if result.CapMinor != 10000 {
		t.Fatalf("CapMinor = %d, want 10000", result.CapMinor)
	}
	if result.BreakerTripped || !result.SwitchEnabled {
		t.Fatalf("Check = %+v, want no trip and an enabled switch", result)
	}

	// The same value without the candidate ID excludes nothing: the existing
	// row is double counted, so the 3,000 candidate now trips. This proves the
	// exclusion is driven by the supplied proposal ID.
	doubled, err := f.service.Check(ctx, limits.Candidate{
		Currency:   "USD",
		ValueMinor: 3000,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check without ProposalID: %v", err)
	}
	if doubled.Allowed || doubled.Tier != policy.TierT1 || doubled.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("Check without ProposalID = %+v, want T1 daily_cap_exceeded", doubled)
	}
	if doubled.CumulativeMinor != 8000 {
		t.Fatalf("doubled CumulativeMinor = %d, want 8000", doubled.CumulativeMinor)
	}
	if !doubled.BreakerTripped || doubled.SwitchEnabled {
		t.Fatalf("Check without ProposalID = %+v, want a trip and disabled switch", doubled)
	}
}

func TestCheckNoReservationPersistBeforeNext(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	ctx := context.Background()
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	// A 6,000 candidate starts proposed, so it is not in the cumulative total.
	firstID := f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(6000, "USD", "debit")),
		status:    "proposed",
		createdAt: dayStart.Add(time.Hour),
	})
	first := limits.Candidate{
		ProposalID: firstID,
		Currency:   "USD",
		ValueMinor: 6000,
		Tier:       policy.TierT0,
	}

	// Check has no reservation: the same unpublished candidate can be checked
	// repeatedly and each call sees cumulative zero.
	for i := 0; i < 2; i++ {
		result, err := f.service.Check(ctx, first)
		if err != nil {
			t.Fatalf("Check(first) #%d: %v", i+1, err)
		}
		if !result.Allowed || result.Reason != limits.ReasonWithinLimits || result.CumulativeMinor != 0 {
			t.Fatalf("Check(first) #%d = %+v, want within_limits with cumulative 0", i+1, result)
		}
	}

	// M4-11 must persist the first auto-approved result before checking the
	// next candidate. Simulate that persistence and check a second 6,000.
	f.setProposalStatus(t, firstID, "auto_approved")
	second, err := f.service.Check(ctx, limits.Candidate{
		Currency:   "USD",
		ValueMinor: 6000,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check(second): %v", err)
	}
	if second.Allowed || second.Tier != policy.TierT1 || second.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("Check(second) = %+v, want T1 daily_cap_exceeded", second)
	}
	if second.CumulativeMinor != 6000 {
		t.Fatalf("second CumulativeMinor = %d, want 6000 from the persisted row", second.CumulativeMinor)
	}
	if !second.BreakerTripped || second.SwitchEnabled {
		t.Fatalf("Check(second) = %+v, want a trip and disabled switch", second)
	}
}

func TestCheckZeroValueCandidateAtCapTrips(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(10000, "USD", "debit")),
		createdAt: dayStart.Add(time.Hour),
	})

	result, err := f.service.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 0,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("Check = %+v, want daily_cap_exceeded", result)
	}
	if result.CumulativeMinor != 10000 || result.CapMinor != 10000 {
		t.Fatalf("Check = %+v, want cumulative and cap 10000", result)
	}
	if !result.BreakerTripped {
		t.Fatal("zero-value candidate at the cap did not trip the breaker")
	}
}

func TestCheckMissingCurrencyCap(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")

	result, err := f.service.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "GBP",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonCurrencyCapMissing {
		t.Fatalf("Check = %+v, want T1 currency_cap_missing", result)
	}
	if result.CumulativeMinor != 0 || result.CapMinor != 0 {
		t.Fatalf("Check = %+v, want zero cumulative/cap", result)
	}
	if result.BreakerTripped || !result.SwitchEnabled {
		t.Fatalf("Check = %+v, want no trip and an enabled switch", result)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("missing cap wrote %d audits", got)
	}
}

func TestCheckMalformedPayloadFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	f.seedProposal(t, proposalSpec{
		payload:   `{"lines":{}}`,
		createdAt: dayStart.Add(time.Hour),
	})

	result, err := f.service.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonLimitsUnavailable {
		t.Fatalf("Check = %+v, want T1 limits_unavailable", result)
	}
	if result.BreakerTripped || !result.SwitchEnabled {
		t.Fatalf("Check = %+v, want no trip and an enabled switch", result)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("malformed payload wrote %d audits", got)
	}
}

func TestCheckBreakRateSpikeTrips(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	// 18 active matches + 2 breaks = 20 events, exactly 1,000 bps.
	f.seedMatches(t, 18, dayStart.Add(2*time.Hour))
	f.seedBreaks(t, 2, dayStart.Add(3*time.Hour), "open")

	result, err := f.service.Check(context.Background(), limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonBreakRateSpike {
		t.Fatalf("Check = %+v, want T1 break_rate_spike", result)
	}
	if !result.BreakerTripped || result.SwitchEnabled {
		t.Fatalf("Check = %+v, want a trip and disabled switch", result)
	}
	row := f.readSwitch(t)
	if row.Enabled || row.Reason != limits.ReasonBreakRateSpike || row.UpdatedBy != limits.CircuitBreakerActor {
		t.Fatalf("switch row = %+v, want the breaker's break_rate_spike trip", row)
	}
	if got := f.auditCount(t); got != 1 {
		t.Fatalf("spike wrote %d switch audits, want 1", got)
	}
	audit := f.readAudit(t)
	if got := audit.After["reason"]; got != limits.ReasonBreakRateSpike {
		t.Fatalf("audit after reason = %v, want %s", got, limits.ReasonBreakRateSpike)
	}
}

func TestCheckBreakRateBelowMinSampleOrThreshold(t *testing.T) {
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	t.Run("below min sample", func(t *testing.T) {
		f := newFixture(t)
		f.setSwitch(t, true, "operator-on")
		// 19 breaks is 100%, but the 20-event minimum is not met.
		f.seedBreaks(t, 19, dayStart.Add(3*time.Hour), "in_case")

		result, err := f.service.Check(context.Background(), limits.Candidate{
			ProposalID: testCandidateID,
			Currency:   "USD",
			ValueMinor: 100,
			Tier:       policy.TierT0,
		})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !result.Allowed || result.Reason != limits.ReasonWithinLimits {
			t.Fatalf("Check = %+v, want within_limits below the minimum sample", result)
		}
		if row := f.readSwitch(t); !row.Enabled {
			t.Fatalf("below-min check tripped the breaker: %+v", row)
		}
	})

	t.Run("below threshold and previous day ignored", func(t *testing.T) {
		f := newFixture(t)
		f.setSwitch(t, true, "operator-on")
		// Today: 19 matches + 1 break = 20 events, 500 bps.
		f.seedMatches(t, 19, dayStart.Add(2*time.Hour))
		f.seedBreaks(t, 1, dayStart.Add(3*time.Hour), "open")
		// Yesterday: would exceed the threshold if the window leaked.
		f.seedMatches(t, 10, dayStart.Add(-12*time.Hour))
		f.seedBreaks(t, 10, dayStart.Add(-12*time.Hour), "open")

		result, err := f.service.Check(context.Background(), limits.Candidate{
			ProposalID: testCandidateID,
			Currency:   "USD",
			ValueMinor: 100,
			Tier:       policy.TierT0,
		})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !result.Allowed || result.Reason != limits.ReasonWithinLimits {
			t.Fatalf("Check = %+v, want within_limits for a normal mix", result)
		}
		if row := f.readSwitch(t); !row.Enabled {
			t.Fatalf("normal mix tripped the breaker: %+v", row)
		}
	})
}

func TestCheckCapTripAuditAndIdempotent(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	dayStart := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	f.seedProposal(t, proposalSpec{
		payload:   linesJSON(lineJSON(9500, "USD", "debit")),
		createdAt: dayStart.Add(time.Hour),
	})
	candidate := limits.Candidate{
		ProposalID: testCandidateID,
		Currency:   "USD",
		ValueMinor: 1000,
		Tier:       policy.TierT0,
	}

	result, err := f.service.Check(context.Background(), candidate)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Allowed || result.Tier != policy.TierT1 || result.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("Check = %+v, want T1 daily_cap_exceeded", result)
	}
	if !result.BreakerTripped || result.SwitchEnabled {
		t.Fatalf("Check = %+v, want a trip and disabled switch", result)
	}

	row := f.readSwitch(t)
	if row.Enabled {
		t.Fatal("switch stayed enabled after the cap trip")
	}
	if row.Reason != limits.ReasonDailyCapExceeded {
		t.Fatalf("switch reason = %q, want %q", row.Reason, limits.ReasonDailyCapExceeded)
	}
	if row.UpdatedBy != limits.CircuitBreakerActor {
		t.Fatalf("switch updated_by = %q, want %q", row.UpdatedBy, limits.CircuitBreakerActor)
	}
	if !row.UpdatedAt.Equal(testNow) {
		t.Fatalf("switch updated_at = %v, want %v", row.UpdatedAt, testNow)
	}

	if got := f.auditCount(t); got != 1 {
		t.Fatalf("switch audits = %d, want 1", got)
	}
	audit := f.readAudit(t)
	if audit.ActorType != "system" || audit.ActorID != "circuit-breaker" {
		t.Fatalf("audit actor = %s:%s, want system:circuit-breaker", audit.ActorType, audit.ActorID)
	}
	if audit.Action != limits.ActionSwitchSet || audit.EntityType != limits.EntitySystemSwitch || audit.EntityID != limits.SwitchAutoApply {
		t.Fatalf("audit identity = %s/%s/%s", audit.Action, audit.EntityType, audit.EntityID)
	}
	if got := audit.Before["enabled"]; got != true {
		t.Fatalf("audit before enabled = %v, want true", got)
	}
	if got := audit.Before["reason"]; got != "operator-on" {
		t.Fatalf("audit before reason = %v, want operator-on", got)
	}
	if got := audit.After["enabled"]; got != false {
		t.Fatalf("audit after enabled = %v, want false", got)
	}
	if got := audit.After["reason"]; got != limits.ReasonDailyCapExceeded {
		t.Fatalf("audit after reason = %v, want %s", got, limits.ReasonDailyCapExceeded)
	}

	// The next T0 check sees the disabled switch and does not trip/audit again.
	second, err := f.service.Check(context.Background(), candidate)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if second.Allowed || second.Reason != limits.ReasonAutoApplyDisabled || second.BreakerTripped {
		t.Fatalf("second Check = %+v, want auto_apply_disabled without a trip", second)
	}
	if got := f.auditCount(t); got != 1 {
		t.Fatalf("second Check wrote an audit: %d rows", got)
	}

	tripped, err := f.service.TripBreaker(context.Background(), limits.ReasonDailyCapExceeded)
	if err != nil {
		t.Fatalf("TripBreaker already off: %v", err)
	}
	if tripped {
		t.Fatal("TripBreaker reported a change while the switch was already off")
	}
	if got := f.auditCount(t); got != 1 {
		t.Fatalf("already-off TripBreaker wrote an audit: %d rows", got)
	}
}

func TestTripBreakerConcurrentSingleAudit(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")

	const callers = 8
	var (
		wg      sync.WaitGroup
		results [callers]bool
		errs    [callers]error
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.service.TripBreaker(context.Background(), limits.ReasonDailyCapExceeded)
		}(i)
	}
	close(start)
	wg.Wait()

	changed := 0
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("TripBreaker caller %d: %v", i, errs[i])
		}
		if results[i] {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("TripBreaker changes = %d, want exactly 1", changed)
	}
	if got := f.auditCount(t); got != 1 {
		t.Fatalf("concurrent trips wrote %d audits, want 1", got)
	}
	if row := f.readSwitch(t); row.Enabled {
		t.Fatalf("switch stayed enabled after concurrent trips: %+v", row)
	}
}

func TestTripBreakerRequiresReason(t *testing.T) {
	f := newFixture(t)
	f.setSwitch(t, true, "operator-on")
	if _, err := f.service.TripBreaker(context.Background(), "   "); !errors.Is(err, limits.ErrTripReason) {
		t.Fatalf("TripBreaker(blank) error = %v, want ErrTripReason", err)
	}
	if row := f.readSwitch(t); !row.Enabled {
		t.Fatalf("blank reason changed the switch: %+v", row)
	}
	if got := f.auditCount(t); got != 0 {
		t.Fatalf("blank reason wrote %d audits", got)
	}
}
