// Package tools defines the contract, registry, validation and execution
// wrapper for the reconciliation agent's tools.
//
// A Tool exposes a name, a description, a JSON Schema for its arguments, and a
// Call method. The Registry compiles and validates the schema once at
// registration time. The Runner executes a call, applies the result size cap,
// records the attempt in tool_calls, and returns a structured JSON envelope
// that is safe to hand to the model.
//
// Two database pools are involved. Tool bodies receive a RunContext whose
// Agent queryer is backed by the read-only recon_agent_ro pool. Recording is
// performed by a separate Recorder backed by recon_app; handlers and
// RunContext never expose the app pool. A handler therefore cannot write tool
// trace rows or reach base tables beyond the granted v_agent_* views.
//
// Tool arguments and results are never logged at info level: they can contain
// memos, bank details or other untrusted payloads. At most the tool name,
// duration and structured error code may be logged by callers.
//
// The package registers no tools itself. M2-05 adds RegisterPart1 and M2-06
// adds RegisterPart2; the agent loop composes them explicitly. There are no
// init-time registration side effects.
package tools
