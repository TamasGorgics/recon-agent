package telemetry

import "testing"

func TestAttributeKeysMatchSpec(t *testing.T) {
	expected := map[string]string{
		"service.name":                  AttrServiceName,
		"service.version":               AttrServiceVersion,
		"deployment.environment":        AttrDeploymentEnvironment,
		"recon.case.id":                 AttrCaseID,
		"recon.run.id":                  AttrRunID,
		"gen_ai.request.model":          AttrModel,
		"recon.prompt.version":          AttrPromptVersion,
		"recon.case.version":            AttrCaseVersion,
		"recon.run.outcome":             AttrRunOutcome,
		"recon.proposal.id":             AttrProposalID,
		"recon.tokens.input":            AttrTokensInput,
		"recon.tokens.output":           AttrTokensOutput,
		"recon.cost.micro_usd":          AttrCostMicroUSD,
		"recon.cost.currency":           AttrCostCurrency,
		"gen_ai.response.model":         AttrResponseModel,
		"gen_ai.usage.input_tokens":     AttrUsageInputTokens,
		"gen_ai.usage.output_tokens":    AttrUsageOutputTokens,
		"recon.llm.seq":                 AttrLLMSeq,
		"recon.request.hash":            AttrRequestHash,
		"recon.response.hash":           AttrResponseHash,
		"recon.response.payload":        AttrResponsePayload,
		"recon.tool.name":               AttrToolName,
		"recon.tool.seq":                AttrToolSeq,
		"recon.tool.error_code":         AttrToolErrorCode,
		"recon.result.hash":             AttrResultHash,
		"recon.refs":                    AttrRefs,
		"recon.flags":                   AttrFlags,
		"recon.payload":                 AttrPayload,
		"recon.policy.tier":             AttrPolicyTier,
		"recon.policy.rule":             AttrPolicyRule,
		"recon.policy.reasons":          AttrPolicyReasons,
		"recon.policy.pattern_verified": AttrPolicyPatternVerified,
		"recon.policy.config_hash":      AttrPolicyConfigHash,
		"recon.policy.limits_reason":    AttrPolicyLimitsReason,
		"recon.proposal.type":           AttrProposalType,
		"recon.apply.status":            AttrApplyStatus,
		"recon.ledger.txn_id":           AttrLedgerTxnID,
		"recon.failure.reason":          AttrFailureReason,
		"recon.executor.run_id":         AttrExecutorRunID,
		"recon.idempotency.hash":        AttrIdempotencyHash,
		"recon.origin.trace_id":         AttrOriginTraceID,
	}
	seen := map[string]bool{}
	for want, got := range expected {
		if got != want {
			t.Errorf("attribute key = %q, want %q", got, want)
		}
		if seen[got] {
			t.Errorf("duplicate attribute key %q", got)
		}
		seen[got] = true
	}
}
