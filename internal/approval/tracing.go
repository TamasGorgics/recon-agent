package approval

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// evaluatePolicy runs the pure policy engine and the FIN-53 limits mapping
// inside the policy.evaluate span. It returns the span-bearing context so the
// later audit append inherits the same trace ID even though the span itself has
// ended before the decision transaction starts.
func (s *Service) evaluatePolicy(
	ctx context.Context,
	proposal *loadedProposal,
	verification policy.Verification,
	facts policy.Facts,
) (context.Context, policy.Decision, string, error) {
	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrProposalID, proposal.ID),
		attribute.String(telemetry.AttrCaseID, proposal.CaseID),
	}
	if proposal.PatternVerified != nil {
		attrs = append(attrs, attribute.Bool(telemetry.AttrPolicyPatternVerified, true))
	}
	ctx, span := telemetry.StartSpan(ctx, s.tracer, "policy.evaluate", attrs...)
	defer span.End()

	decision := s.policy.Engine.Evaluate(policy.Proposal{
		ID:              proposal.ID,
		CaseID:          proposal.CaseID,
		Type:            policy.ProposalType(proposal.Type),
		Confidence:      proposal.Confidence,
		Evidence:        append([]string(nil), proposal.Evidence...),
		PatternVerified: proposal.PatternVerified,
	}, verification, facts)
	if err := decision.Validate(); err != nil {
		telemetry.RecordError(span, err)
		return ctx, policy.Decision{}, "", fmt.Errorf("%w: policy decision: %v", ErrInvalidRequest, err)
	}

	limitsReason := ""
	if decision.Tier == policy.TierT0 {
		result, checkErr := s.limits.Check(ctx, limits.Candidate{
			ProposalID: proposal.ID,
			CaseID:     proposal.CaseID,
			Currency:   facts.Currency,
			ValueMinor: facts.ValueMinor,
			Tier:       decision.Tier,
		})
		if checkErr != nil {
			// FIN-53 fails closed: a limits infrastructure failure can never
			// auto-apply. The exact error is not stored; a closed machine code
			// is audited instead.
			limitsReason = limits.ReasonLimitsUnavailable
			decision = mapLimitsT1(decision, limitsReason)
		} else {
			limitsReason = result.Reason
			if !result.Allowed || result.Tier != policy.TierT0 ||
				result.Reason != limits.ReasonWithinLimits {
				decision = mapLimitsT1(decision, result.Reason)
			}
		}
	}

	span.SetAttributes(
		attribute.String(telemetry.AttrPolicyTier, string(decision.Tier)),
		attribute.String(telemetry.AttrPolicyRule, decision.Rule),
		attribute.StringSlice(telemetry.AttrPolicyReasons, decision.Reasons),
		attribute.String(telemetry.AttrPolicyConfigHash, decision.ConfigHash),
	)
	if limitsReason != "" {
		span.SetAttributes(attribute.String(telemetry.AttrPolicyLimitsReason, limitsReason))
	}
	if origin := originTraceID(ctx, proposal.TraceID); origin != "" {
		span.SetAttributes(attribute.String(telemetry.AttrOriginTraceID, origin))
	}
	return ctx, decision, limitsReason, nil
}

// originTraceID returns the persisted proposal trace ID when it is valid and
// differs from the span's own trace. A bare trace ID is never turned into a
// synthetic parent; correlation is an attribute only.
func originTraceID(ctx context.Context, persisted *string) string {
	if persisted == nil || !telemetry.ValidTraceID(*persisted) {
		return ""
	}
	if *persisted == telemetry.TraceID(ctx) {
		return ""
	}
	return *persisted
}
