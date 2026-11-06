// Package agent drives the LLM tool loop that investigates reconciliation cases.
//
// # Run contract
//
// One Run investigates one leased case. It inserts an agent_runs row, seeds the
// transcript with a recorded get_case call, then alternates model completions
// and tool calls until a terminal outcome. Every completion is persisted in
// llm_calls with a canonical request hash and every attempted tool call in
// tool_calls. Model calls never hold a database transaction.
//
// # Outcomes and case transitions
//
// The five persisted outcomes are proposal, escalated, budget_exhausted,
// invalid_output, and provider_error. A successful proposal tool moves the case
// to proposal_pending (waiting_info for request_info). Escalation and injection
// flags move it to escalated with the tool reason or injection_flagged. Loop
// detection also escalates with repeated_identical_tool_call. budget_exhausted
// and invalid_output leave the case leased: the worker releases it and records
// the attempt. provider_error leaves it leased without an attempt. A disabled
// kill switch finalizes the run escalated and does not transition the case.
//
// # Budgets and repairs
//
// Steps, executed tool calls, total tokens, cost, and wall clock are bounded;
// the first exhausted check stops before the next completion or tool call.
// Monetary accounting uses shopspring/decimal. One protocol repair is allowed
// per run for malformed tool arguments or a completion with no tool call; a
// second protocol failure ends invalid_output. Business rejections and
// structured tool errors are returned to the model and never consume the
// repair slot.
//
// # Stale transitions
//
// Terminal transitions carry the case version and leased_by owner read at run
// start. On cases.ErrStale the loop re-reads the case and retries once only
// when it is still in_agent with the same owner; a moved status or owner is
// never overwritten and the result reports LeaseLost.
package agent
