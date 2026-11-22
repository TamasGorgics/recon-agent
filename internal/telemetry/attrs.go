package telemetry

// Attribute keys shared by every M5-01 span. Keeping them here prevents name
// drift between instrumentation sites and tests.
const (
	// Resource attributes.
	AttrServiceName           = "service.name"
	AttrServiceVersion        = "service.version"
	AttrDeploymentEnvironment = "deployment.environment"

	// case.run.
	AttrCaseID        = "recon.case.id"
	AttrRunID         = "recon.run.id"
	AttrModel         = "gen_ai.request.model"
	AttrPromptVersion = "recon.prompt.version"
	AttrCaseVersion   = "recon.case.version"
	AttrRunOutcome    = "recon.run.outcome"
	AttrProposalID    = "recon.proposal.id"
	AttrTokensInput   = "recon.tokens.input"
	AttrTokensOutput  = "recon.tokens.output"

	// Cost is integer micro-USD; no floats are ever recorded.
	AttrCostMicroUSD = "recon.cost.micro_usd"
	AttrCostCurrency = "recon.cost.currency"

	// llm.call.
	AttrResponseModel     = "gen_ai.response.model"
	AttrUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens = "gen_ai.usage.output_tokens"
	AttrLLMSeq            = "recon.llm.seq"
	AttrRequestHash       = "recon.request.hash"
	AttrResponseHash      = "recon.response.hash"
	AttrResponsePayload   = "recon.response.payload"

	// tool.call.
	AttrToolName      = "recon.tool.name"
	AttrToolSeq       = "recon.tool.seq"
	AttrToolErrorCode = "recon.tool.error_code"
	AttrResultHash    = "recon.result.hash"
	AttrRefs          = "recon.refs"
	AttrFlags         = "recon.flags"
	AttrPayload       = "recon.payload"

	// policy.evaluate.
	AttrPolicyTier            = "recon.policy.tier"
	AttrPolicyRule            = "recon.policy.rule"
	AttrPolicyReasons         = "recon.policy.reasons"
	AttrPolicyPatternVerified = "recon.policy.pattern_verified"
	AttrPolicyConfigHash      = "recon.policy.config_hash"
	AttrPolicyLimitsReason    = "recon.policy.limits_reason"

	// executor.apply.
	AttrProposalType    = "recon.proposal.type"
	AttrApplyStatus     = "recon.apply.status"
	AttrLedgerTxnID     = "recon.ledger.txn_id"
	AttrFailureReason   = "recon.failure.reason"
	AttrExecutorRunID   = "recon.executor.run_id"
	AttrIdempotencyHash = "recon.idempotency.hash"

	// Cross-process correlation only; never a fabricated parent span.
	AttrOriginTraceID = "recon.origin.trace_id"
)
