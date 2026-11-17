//go:build integration

// covers: FIN-30, FIN-53
package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

func TestRequestRoutesT1ToSlack(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, caseID, _ := f.seedProposal(proposalSpec{
		Type:            "adjustment",
		Payload:         adjustmentPayload(100),
		PatternVerified: strPtr(policy.PatternFeeVariance),
		PatternClaimed:  strPtr(policy.PatternFeeVariance),
	})

	result, err := f.service.Request(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	wantExpiry := f.testNowUTC.Add(testTTL)
	assertResult(t, result, approval.RequestResult{
		ProposalID:   proposalID,
		Tier:         policy.TierT1,
		Status:       "pending_approval",
		Channel:      approval.ChannelSlack,
		ExpiresAt:    wantExpiry,
		AutoApproved: false,
	})
	if f.limits.calls != 0 {
		t.Fatalf("limits.Check calls = %d, want 0 for a policy T1", f.limits.calls)
	}
	if f.verifier.calls != 1 || f.verifier.proposalIDs[0] != proposalID {
		t.Fatalf("verifier calls = %d/%v, want one call for %s",
			f.verifier.calls, f.verifier.proposalIDs, proposalID)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "pending_approval" || stored.Version != 1 {
		t.Fatalf("stored status/version = %s/%d, want pending_approval/1", stored.Status, stored.Version)
	}
	if stored.Tier == nil || *stored.Tier != "T1" {
		t.Fatalf("risk_tier = %v, want T1", stored.Tier)
	}
	if !reflect.DeepEqual(f.decisionOf(stored), f.engine.decision) {
		t.Fatalf("persisted decision = %+v, want %+v", f.decisionOf(stored), f.engine.decision)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expires_at = %v, want %v", stored.ExpiresAt, wantExpiry)
	}

	assertTierAudit(t, f, proposalID, policy.TierT1, "verifier_failed", "", "slack")
	assertApprovalOutbox(t, f, proposalID, caseID, "T1", "slack")

	// The service has no channel dependency in wave 1a: the outbox row is the
	// only hand-off, so no FakeChannel/network can have been used.
	if _, ok := f.auditByAction(proposalID, approval.ActionProposalPolicy); !ok {
		t.Fatal("proposal.policy audit is missing")
	}
}

func TestRequestRoutesT2ToTicket(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT2)
	proposalID, caseID, _ := f.seedProposal(proposalSpec{
		Type:    "link",
		Payload: linkPayload(),
	})

	result, err := f.service.Request(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if result.Tier != policy.TierT2 || result.Status != "pending_approval" ||
		result.Channel != approval.ChannelTicket || result.AutoApproved {
		t.Fatalf("Request result = %+v, want T2 pending_approval/ticket", result)
	}
	if f.limits.calls != 0 {
		t.Fatalf("limits.Check calls = %d, want 0 for a policy T2", f.limits.calls)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "pending_approval" || stored.Tier == nil || *stored.Tier != "T2" {
		t.Fatalf("stored proposal = %s/%v, want pending_approval/T2", stored.Status, stored.Tier)
	}
	assertTierAudit(t, f, proposalID, policy.TierT2, "high_value", "", "ticket")
	assertApprovalOutbox(t, f, proposalID, caseID, "T2", "ticket")
}

func TestRequestT0AllowedAutoApprovesWithoutOutbox(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT0)
	f.limits.result = limits.Result{
		Allowed:         true,
		Tier:            policy.TierT0,
		Reason:          limits.ReasonWithinLimits,
		CumulativeMinor: 0,
		CapMinor:        10_000,
		SwitchEnabled:   true,
	}
	proposalID, caseID, _ := f.seedProposal(proposalSpec{
		Type:            "adjustment",
		Payload:         adjustmentPayload(100),
		PatternVerified: strPtr(policy.PatternFeeVariance),
		PatternClaimed:  strPtr(policy.PatternFeeVariance),
	})

	result, err := f.service.Request(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if result.Tier != policy.TierT0 || result.Status != "auto_approved" ||
		!result.AutoApproved || result.Channel != "" ||
		result.LimitsReason != limits.ReasonWithinLimits {
		t.Fatalf("Request result = %+v, want T0 auto_approved/within_limits/no channel", result)
	}
	if f.limits.calls != 1 {
		t.Fatalf("limits.Check calls = %d, want exactly 1", f.limits.calls)
	}
	wantCandidate := limits.Candidate{
		ProposalID: proposalID,
		CaseID:     caseID,
		Currency:   "USD",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	}
	if f.limits.candidates[0] != wantCandidate {
		t.Fatalf("limits candidate = %+v, want %+v", f.limits.candidates[0], wantCandidate)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "auto_approved" || stored.Version != 1 {
		t.Fatalf("stored status/version = %s/%d, want auto_approved/1", stored.Status, stored.Version)
	}
	if stored.Tier == nil || *stored.Tier != "T0" {
		t.Fatalf("risk_tier = %v, want T0", stored.Tier)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(f.testNowUTC.Add(testTTL)) {
		t.Fatalf("expires_at = %v, want %v", stored.ExpiresAt, f.testNowUTC.Add(testTTL))
	}
	if rows := f.outboxRows(proposalID); len(rows) != 0 {
		t.Fatalf("T0 outbox rows = %d, want 0", len(rows))
	}
	assertTierAudit(t, f, proposalID, policy.TierT0, "auto_apply", "within_limits", "")
}

func TestRequestT0LimitsT1MapsDecision(t *testing.T) {
	tests := []struct {
		name       string
		limitsRes  limits.Result
		limitsErr  error
		wantRule   string
		wantLimits string
	}{
		{
			name:       "auto apply disabled",
			limitsRes:  limits.Result{Allowed: false, Tier: policy.TierT1, Reason: limits.ReasonAutoApplyDisabled},
			wantRule:   policy.CodeAutoApplyDisabled,
			wantLimits: limits.ReasonAutoApplyDisabled,
		},
		{
			name:       "daily cap exceeded",
			limitsRes:  limits.Result{Allowed: false, Tier: policy.TierT1, Reason: limits.ReasonDailyCapExceeded},
			wantRule:   policy.CodeDailyCapExceeded,
			wantLimits: limits.ReasonDailyCapExceeded,
		},
		{
			name:       "break rate spike",
			limitsRes:  limits.Result{Allowed: false, Tier: policy.TierT1, Reason: limits.ReasonBreakRateSpike},
			wantRule:   policy.CodeBreakRateSpike,
			wantLimits: limits.ReasonBreakRateSpike,
		},
		{
			name:       "other reason maps to value unknown",
			limitsRes:  limits.Result{Allowed: false, Tier: policy.TierT1, Reason: limits.ReasonCurrencyCapMissing},
			wantRule:   policy.CodeValueUnknown,
			wantLimits: limits.ReasonCurrencyCapMissing,
		},
		{
			name:       "limits error fails closed",
			limitsErr:  errors.New("limits database unavailable"),
			wantRule:   policy.CodeValueUnknown,
			wantLimits: limits.ReasonLimitsUnavailable,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.engine.decision = decisionForTier(t, f.loaded, policy.TierT0)
			f.limits.result = tc.limitsRes
			f.limits.err = tc.limitsErr
			proposalID, caseID, _ := f.seedProposal(proposalSpec{
				Type:            "adjustment",
				Payload:         adjustmentPayload(100),
				PatternVerified: strPtr(policy.PatternFeeVariance),
				PatternClaimed:  strPtr(policy.PatternFeeVariance),
			})

			result, err := f.service.Request(context.Background(), proposalID)
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if result.Tier != policy.TierT1 || result.Status != "pending_approval" ||
				result.AutoApproved || result.Channel != approval.ChannelSlack ||
				result.LimitsReason != tc.wantLimits {
				t.Fatalf("Request result = %+v, want T1 pending/slack/%s", result, tc.wantLimits)
			}
			if f.limits.calls != 1 {
				t.Fatalf("limits.Check calls = %d, want 1", f.limits.calls)
			}

			stored := f.readProposal(proposalID)
			decision := f.decisionOf(stored)
			if stored.Tier == nil || *stored.Tier != "T1" {
				t.Fatalf("risk_tier = %v, want T1", stored.Tier)
			}
			if decision.Tier != policy.TierT1 || decision.Rule != tc.wantRule ||
				!reflect.DeepEqual(decision.Reasons, []string{tc.wantRule}) {
				t.Fatalf("persisted decision = %+v, want T1 rule %s", decision, tc.wantRule)
			}
			if decision.ConfigHash != f.loaded.Hash {
				t.Fatalf("decision config hash = %s, want %s", decision.ConfigHash, f.loaded.Hash)
			}
			assertTierAudit(t, f, proposalID, policy.TierT1, tc.wantRule, tc.wantLimits, "slack")
			assertApprovalOutbox(t, f, proposalID, caseID, "T1", "slack")
		})
	}
}

func TestRequestBuildsPolicyFacts(t *testing.T) {
	cases := []struct {
		name     string
		spec     proposalSpec
		wantType policy.ProposalType
		want     policy.Facts
	}{
		{
			name: "adjustment debit total and lines",
			spec: proposalSpec{
				Type: "adjustment",
				Payload: map[string]any{"lines": []map[string]any{
					{"account_code": "clearing_suspense", "direction": "debit", "amount_minor": 120, "currency": "USD"},
					{"account_code": "clearing_fees", "direction": "debit", "amount_minor": 30, "currency": "USD"},
					{"account_code": "fees_expense", "direction": "credit", "amount_minor": 150, "currency": "USD"},
				}},
			},
			wantType: policy.TypeAdjustment,
			want: policy.Facts{
				ValueMinor:       150,
				Currency:         "USD",
				CaseCurrency:     "USD",
				LineCurrencies:   []string{"USD", "USD", "USD"},
				AccountCodes:     []string{"clearing_suspense", "clearing_fees", "fees_expense"},
				EvidenceValid:    true,
				AutoApplyEnabled: true,
			},
		},
		{
			name: "write off uses the same limit facts",
			spec: proposalSpec{
				Type:    "write_off",
				Payload: adjustmentPayload(75),
			},
			wantType: policy.TypeWriteOff,
			want: policy.Facts{
				ValueMinor:       75,
				Currency:         "USD",
				CaseCurrency:     "USD",
				LineCurrencies:   []string{"USD", "USD"},
				AccountCodes:     []string{"clearing_suspense", "fees_expense"},
				EvidenceValid:    true,
				AutoApplyEnabled: true,
			},
		},
		{
			name:     "link is zero value in the case currency",
			spec:     proposalSpec{Type: "link", Payload: linkPayload()},
			wantType: policy.TypeLink,
			want: policy.Facts{
				Currency:         "USD",
				CaseCurrency:     "USD",
				EvidenceValid:    true,
				AutoApplyEnabled: true,
			},
		},
		{
			name:     "duplicate is zero value in the case currency",
			spec:     proposalSpec{Type: "duplicate", Payload: linkPayload()},
			wantType: policy.TypeDuplicate,
			want: policy.Facts{
				Currency:         "USD",
				CaseCurrency:     "USD",
				EvidenceValid:    true,
				AutoApplyEnabled: true,
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
			proposalID, caseID, _ := f.seedProposal(tc.spec)

			if _, err := f.service.Request(context.Background(), proposalID); err != nil {
				t.Fatalf("Request: %v", err)
			}
			if f.engine.calls != 1 {
				t.Fatalf("policy Evaluate calls = %d, want 1", f.engine.calls)
			}
			if !reflect.DeepEqual(f.engine.facts, tc.want) {
				t.Fatalf("policy facts = %+v, want %+v", f.engine.facts, tc.want)
			}
			if f.engine.proposal.Type != tc.wantType {
				t.Fatalf("policy proposal type = %q, want %q", f.engine.proposal.Type, tc.wantType)
			}
			if f.engine.proposal.ID != proposalID || f.engine.proposal.CaseID != caseID {
				t.Fatalf("policy proposal ids = %s/%s, want %s/%s",
					f.engine.proposal.ID, f.engine.proposal.CaseID, proposalID, caseID)
			}
			if f.engine.proposal.Confidence.String() != "0.9" {
				t.Fatalf("policy confidence = %s, want 0.9", f.engine.proposal.Confidence)
			}
			if len(f.engine.proposal.Evidence) == 0 {
				t.Fatal("policy proposal evidence is empty")
			}
			if f.engine.verification.Pattern != policy.PatternFeeVariance || !f.engine.verification.Passed {
				t.Fatalf("policy verification = %+v, want the verifier hand-off", f.engine.verification)
			}
		})
	}
}

// TestRequestReloadsPatternVerifiedAfterVerifier is the blocker regression: an
// agent-created proposal has pattern_verified NULL until Verify persists it.
// Request must reload the column before handing the proposal to policy.
func TestRequestReloadsPatternVerifiedAfterVerifier(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:    "adjustment",
		Payload: adjustmentPayload(100),
	})
	f.verifier.onVerify = func(ctx context.Context, id string) {
		if _, err := f.app.Exec(ctx,
			`UPDATE proposals SET pattern_verified = 'fee_variance' WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("persist verifier pattern: %v", err)
		}
	}

	if _, err := f.service.Request(context.Background(), proposalID); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if f.engine.proposal.PatternVerified == nil || *f.engine.proposal.PatternVerified != policy.PatternFeeVariance {
		t.Fatalf("policy pattern_verified = %v, want fee_variance", f.engine.proposal.PatternVerified)
	}
}

func TestRequestAutoApplySwitchDisabledIsAFact(t *testing.T) {
	f := newFixture(t)
	f.setAutoApply(false)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:            "adjustment",
		Payload:         adjustmentPayload(100),
		PatternVerified: strPtr(policy.PatternFeeVariance),
		PatternClaimed:  strPtr(policy.PatternFeeVariance),
	})

	if _, err := f.service.Request(context.Background(), proposalID); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if f.engine.facts.AutoApplyEnabled {
		t.Fatal("AutoApplyEnabled = true, want false from the disabled switch")
	}
}

func TestRequestReRequestIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:            "adjustment",
		Payload:         adjustmentPayload(100),
		PatternVerified: strPtr(policy.PatternFeeVariance),
		PatternClaimed:  strPtr(policy.PatternFeeVariance),
	})

	if _, err := f.service.Request(context.Background(), proposalID); err != nil {
		t.Fatalf("first Request: %v", err)
	}
	before := f.readProposal(proposalID)
	auditsBefore := len(f.audits(proposalID))
	outboxBefore := len(f.outboxRows(proposalID))
	verifierBefore := f.verifier.calls

	result, err := f.service.Request(context.Background(), proposalID)
	if !errors.Is(err, approval.ErrAlreadyRequested) {
		t.Fatalf("second Request error = %v, want ErrAlreadyRequested", err)
	}
	if result.ProposalID != proposalID || result.Status != "pending_approval" {
		t.Fatalf("second Request result = %+v, want current status", result)
	}
	if f.verifier.calls != verifierBefore {
		t.Fatalf("second Request called the verifier again (%d -> %d)", verifierBefore, f.verifier.calls)
	}
	after := f.readProposal(proposalID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("re-request changed the proposal: before %+v after %+v", before, after)
	}
	if got := len(f.audits(proposalID)); got != auditsBefore {
		t.Fatalf("audit rows after re-request = %d, want %d", got, auditsBefore)
	}
	if got := len(f.outboxRows(proposalID)); got != outboxBefore {
		t.Fatalf("outbox rows after re-request = %d, want %d", got, outboxBefore)
	}

	// A proposal seeded directly in a non-proposed state is also refused before
	// verification or any write.
	directID, _, _ := f.seedProposal(proposalSpec{
		Type:    "link",
		Payload: linkPayload(),
		Status:  "pending_approval",
	})
	verifierBefore = f.verifier.calls
	if _, err := f.service.Request(context.Background(), directID); !errors.Is(err, approval.ErrAlreadyRequested) {
		t.Fatalf("non-proposed Request error = %v, want ErrAlreadyRequested", err)
	}
	if f.verifier.calls != verifierBefore {
		t.Fatal("non-proposed Request called the verifier")
	}
}

func TestRequestDuplicateOutboxKeyFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = decisionForTier(t, f.loaded, policy.TierT1)
	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:    "link",
		Payload: linkPayload(),
	})
	key := approval.TopicApprovalRequest + ":" + proposalID
	if _, err := f.d.Pool.Exec(context.Background(),
		`INSERT INTO outbox
		     (aggregate_type, aggregate_id, topic, payload, idempotency_key,
		      created_at, next_attempt_at)
		 VALUES ('proposal', $1, $2, '{}'::jsonb, $3, now(), now())`,
		proposalID, approval.TopicApprovalRequest, key); err != nil {
		t.Fatalf("pre-insert outbox row: %v", err)
	}

	if _, err := f.service.Request(context.Background(), proposalID); !errors.Is(err, approval.ErrAlreadyRequested) {
		t.Fatalf("Request error = %v, want ErrAlreadyRequested for a duplicate outbox key", err)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "proposed" || stored.Version != 0 || stored.Tier != nil || stored.ExpiresAt != nil {
		t.Fatalf("proposal after failed Request = %+v, want unchanged proposed row", stored)
	}
	if audits := f.audits(proposalID); len(audits) != 0 {
		t.Fatalf("audit rows after failed Request = %d, want 0", len(audits))
	}
	if rows := f.outboxRows(proposalID); len(rows) != 1 {
		t.Fatalf("outbox rows after failed Request = %d, want the pre-existing 1", len(rows))
	}
}

func TestRequestVerifierErrorWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.verifier.err = errors.New("verifier source lookup failed")
	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:    "link",
		Payload: linkPayload(),
	})

	if _, err := f.service.Request(context.Background(), proposalID); err == nil {
		t.Fatal("Request succeeded, want the verifier error")
	} else if !errors.Is(err, f.verifier.err) {
		t.Fatalf("Request error = %v, want the verifier error", err)
	}

	stored := f.readProposal(proposalID)
	if stored.Status != "proposed" || stored.Version != 0 || stored.Tier != nil || stored.ExpiresAt != nil {
		t.Fatalf("proposal after verifier error = %+v, want unchanged", stored)
	}
	if audits := f.audits(proposalID); len(audits) != 0 {
		t.Fatalf("audit rows after verifier error = %d, want 0", len(audits))
	}
	if rows := f.outboxRows(proposalID); len(rows) != 0 {
		t.Fatalf("outbox rows after verifier error = %d, want 0", len(rows))
	}
	if f.engine.calls != 0 || f.limits.calls != 0 {
		t.Fatalf("policy/limits calls after verifier error = %d/%d, want 0/0",
			f.engine.calls, f.limits.calls)
	}
}

func TestRequestRejectsInvalidDecisionAndIDs(t *testing.T) {
	f := newFixture(t)
	f.engine.decision = policy.Decision{
		SchemaVersion: policy.SchemaVersion,
		Tier:          policy.TierT1,
		Rule:          policy.CodeHighValue,
		Reasons:       []string{policy.CodeHighValue},
		ConfigHash:    f.loaded.Hash,
	}
	proposalID, _, _ := f.seedProposal(proposalSpec{Type: "link", Payload: linkPayload()})
	if _, err := f.service.Request(context.Background(), proposalID); !errors.Is(err, approval.ErrInvalidRequest) {
		t.Fatalf("invalid engine decision error = %v, want ErrInvalidRequest", err)
	}
	if stored := f.readProposal(proposalID); stored.Status != "proposed" || stored.Version != 0 {
		t.Fatalf("proposal changed after invalid engine decision: %+v", stored)
	}

	if _, err := f.service.Request(context.Background(), "not-a-uuid"); !errors.Is(err, approval.ErrInvalidRequest) {
		t.Fatalf("malformed id error = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.service.Request(context.Background(),
		"00000000-0000-4000-8000-000000000099"); !errors.Is(err, approval.ErrProposalNotFound) {
		t.Fatalf("missing id error = %v, want ErrProposalNotFound", err)
	}
}

func assertResult(t *testing.T, got, want approval.RequestResult) {
	t.Helper()
	if got.ProposalID != want.ProposalID || got.Tier != want.Tier ||
		got.Status != want.Status || got.Channel != want.Channel ||
		got.AutoApproved != want.AutoApproved || got.LimitsReason != want.LimitsReason ||
		!got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("Request result = %+v, want %+v", got, want)
	}
}

func assertTierAudit(t *testing.T, f *fixture, proposalID string, tier policy.Tier, rule, limitsReason, channel string) {
	t.Helper()
	audits := f.audits(proposalID)
	if len(audits) != 2 {
		t.Fatalf("audit rows = %d, want transition + policy (2): %+v", len(audits), audits)
	}
	transition := audits[0]
	if transition.Action != "proposal.transition" {
		t.Fatalf("first audit action = %q, want proposal.transition", transition.Action)
	}
	if transition.ActorType != "system" || transition.ActorID != "approval" {
		t.Fatalf("transition actor = %s:%s, want system:approval", transition.ActorType, transition.ActorID)
	}
	afterTransition := decodeObject(t, transition.After)
	wantStatus := "pending_approval"
	if tier == policy.TierT0 {
		wantStatus = "auto_approved"
	}
	if afterTransition["status"] != wantStatus {
		t.Fatalf("transition after status = %v, want %s", afterTransition["status"], wantStatus)
	}

	policyAudit := audits[1]
	if policyAudit.Action != approval.ActionProposalPolicy {
		t.Fatalf("second audit action = %q, want %s", policyAudit.Action, approval.ActionProposalPolicy)
	}
	if policyAudit.ActorType != "system" || policyAudit.ActorID != "approval" {
		t.Fatalf("policy actor = %s:%s, want system:approval", policyAudit.ActorType, policyAudit.ActorID)
	}
	after := decodeObject(t, policyAudit.After)
	if after["tier"] != string(tier) {
		t.Fatalf("policy audit tier = %v, want %s", after["tier"], tier)
	}
	if after["rule"] != rule {
		t.Fatalf("policy audit rule = %v, want %s", after["rule"], rule)
	}
	reasons, ok := after["reasons"].([]any)
	if !ok || len(reasons) != 1 || reasons[0] != rule {
		t.Fatalf("policy audit reasons = %v, want [%s]", after["reasons"], rule)
	}
	if after["config_hash"] != f.loaded.Hash {
		t.Fatalf("policy audit config_hash = %v, want %s", after["config_hash"], f.loaded.Hash)
	}
	if after["ttl_seconds"] != float64(48*60*60) {
		t.Fatalf("policy audit ttl_seconds = %v, want 172800", after["ttl_seconds"])
	}
	wantAuto := tier == policy.TierT0
	if after["auto_approved"] != wantAuto {
		t.Fatalf("policy audit auto_approved = %v, want %t", after["auto_approved"], wantAuto)
	}
	if limitsReason == "" {
		if _, present := after["limits_reason"]; present {
			t.Fatalf("policy audit has limits_reason %v, want absent", after["limits_reason"])
		}
	} else if after["limits_reason"] != limitsReason {
		t.Fatalf("policy audit limits_reason = %v, want %s", after["limits_reason"], limitsReason)
	}
	if channel == "" {
		if _, present := after["channel"]; present {
			t.Fatalf("policy audit has channel %v, want absent", after["channel"])
		}
	} else if after["channel"] != channel {
		t.Fatalf("policy audit channel = %v, want %s", after["channel"], channel)
	}

	// A transition audit must carry the pending reason for T1/T2.
	if tier != policy.TierT0 {
		if afterTransition["reason"] != approval.ReasonApprovalRequired {
			t.Fatalf("transition reason = %v, want %s", afterTransition["reason"], approval.ReasonApprovalRequired)
		}
	}
}

func assertApprovalOutbox(t *testing.T, f *fixture, proposalID, caseID, tier, channel string) {
	t.Helper()
	rows := f.outboxRows(proposalID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.AggregateType != "proposal" || row.AggregateID != proposalID {
		t.Fatalf("outbox aggregate = %s/%s, want proposal/%s", row.AggregateType, row.AggregateID, proposalID)
	}
	if row.Topic != approval.TopicApprovalRequest {
		t.Fatalf("outbox topic = %q, want %q", row.Topic, approval.TopicApprovalRequest)
	}
	wantKey := approval.TopicApprovalRequest + ":" + proposalID
	if row.IdempotencyKey != wantKey {
		t.Fatalf("outbox idempotency key = %q, want %q", row.IdempotencyKey, wantKey)
	}
	var payload struct {
		ProposalID  string `json:"proposal_id"`
		CaseID      string `json:"case_id"`
		Tier        string `json:"tier"`
		Channel     string `json:"channel"`
		RequestedAt string `json:"requested_at"`
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("decode approval.request payload: %v", err)
	}
	if payload.ProposalID != proposalID || payload.CaseID != caseID ||
		payload.Tier != tier || payload.Channel != channel {
		t.Fatalf("approval.request payload = %+v, want %s/%s/%s/%s",
			payload, proposalID, caseID, tier, channel)
	}
	if payload.RequestedAt != f.testNowUTC.Format(time.RFC3339Nano) {
		t.Fatalf("approval.request requested_at = %q, want %s",
			payload.RequestedAt, f.testNowUTC.Format(time.RFC3339Nano))
	}
}
