package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// Runner executes registered tool calls. It owns the timing clock and the
// recorder; tool bodies still run against the agent-pool queryer carried by
// RunContext.
type Runner struct {
	registry *Registry
	recorder Recorder
	clock    clock.Clock

	// MaxResultBytes overrides the package default for tests and specialized
	// callers. Zero means MaxResultBytes.
	MaxResultBytes int

	// Tracer is optional. Nil is a no-op; agent.NewRunner and
	// agent.NewProductionRunner set it from the injected telemetry provider.
	Tracer trace.Tracer
}

// NewRunner returns a runner. A nil clock is replaced with clock.Real; callers
// in domain code should always inject a clock.
func NewRunner(registry *Registry, recorder Recorder, clk clock.Clock) *Runner {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Runner{
		registry:       registry,
		recorder:       recorder,
		clock:          clk,
		MaxResultBytes: MaxResultBytes,
	}
}

// Call executes one attempted tool call and returns the final JSON envelope.
//
// Invalid run context (missing run/case ids or seq <= 0) returns an error
// wrapping ErrInvalidRunContext without recording. Every other outcome is
// recorded, including unknown tools, invalid arguments, handler errors and
// handler panics. If recording fails, Call returns an error wrapping
// ErrRecordFailed and no envelope.
func (r *Runner) Call(ctx context.Context, run *RunContext, seq int, name string, args json.RawMessage) (json.RawMessage, error) {
	if run == nil || run.RunID == "" || run.CaseID == "" || seq <= 0 {
		return nil, fmt.Errorf("%w: run id, case id and a positive seq are required", ErrInvalidRunContext)
	}
	if run.Clock == nil {
		run.Clock = r.clock
	}
	startedAt := r.clock.Now()

	body, validJSON := normalizeArgs(args)
	recordedName := name
	if recordedName == "" {
		recordedName = "(unknown)"
	}

	// One tool.call span per attempted call, including unknown tools and
	// invalid arguments. The span-bearing ctx is passed to the handler so
	// proposal inserts and audit appends see the same trace. The raw
	// model-supplied name is never attached: only a registered tool name (after
	// a successful registry lookup) or the bounded "unknown" placeholder is.
	ctx, span := telemetry.StartSpan(ctx, r.Tracer, "tool.call",
		attribute.Int64(telemetry.AttrToolSeq, int64(seq)),
		attribute.String(telemetry.AttrRunID, run.RunID),
		attribute.String(telemetry.AttrCaseID, run.CaseID),
	)

	entry, known := r.registry.lookup(name)
	spanToolName := "unknown"
	if known {
		// name matched a code-registered tool exactly, so it is safe to expose.
		spanToolName = name
	}
	span.SetAttributes(attribute.String(telemetry.AttrToolName, spanToolName))
	if !known {
		envelope := marshalErrorEnvelope(CodeUnknownTool, defaultMessage(CodeUnknownTool))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeUnknownTool, startedAt, nil, nil, span)
	}
	if !validJSON {
		envelope := marshalErrorEnvelope(CodeInvalidArguments, defaultMessage(CodeInvalidArguments))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInvalidArguments, startedAt, nil, nil, span)
	}
	if err := validateArguments(entry.schema, body); err != nil {
		envelope := marshalErrorEnvelope(CodeInvalidArguments, defaultMessage(CodeInvalidArguments))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInvalidArguments, startedAt, nil, nil, span)
	}

	result, handlerErr := callHandler(ctx, entry.tool, run, body)
	if handlerErr != nil {
		code, message := errorPayload(handlerErr)
		envelope := marshalErrorEnvelope(code, message)
		return r.finish(ctx, run, seq, recordedName, body, envelope, code, startedAt, nil, nil, span)
	}

	envelope, refs, err := buildSuccessEnvelope(result, r.maxResultBytes())
	if err != nil {
		// The handler returned data that cannot be serialized or a ref that is
		// not canonical. Do not leak the underlying error to the model.
		envelope = marshalErrorEnvelope(CodeInternalError, defaultMessage(CodeInternalError))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInternalError, startedAt, nil, nil, span)
	}
	// Detection runs after the final envelope exists and before the call is
	// recorded. It never changes the model-visible envelope.
	flags := detectEnvelopeFlags(envelope)
	return r.finish(ctx, run, seq, recordedName, body, envelope, "", startedAt, refs, flags, span)
}

func (r *Runner) finish(
	ctx context.Context,
	run *RunContext,
	seq int,
	name string,
	args json.RawMessage,
	envelope []byte,
	errorCode string,
	startedAt time.Time,
	refs []string,
	flags []string,
	span trace.Span,
) (json.RawMessage, error) {
	resultHash := hashResult(envelope)
	if err := r.record(ctx, run, seq, name, args, envelope, errorCode, startedAt, flags); err != nil {
		telemetry.RecordError(span, err)
		span.End()
		return nil, err
	}
	span.SetAttributes(attribute.String(telemetry.AttrResultHash, resultHash))
	span.SetAttributes(attribute.StringSlice(telemetry.AttrRefs, capRefs(refs)))
	span.SetAttributes(attribute.StringSlice(telemetry.AttrFlags, normalizeFlagCodes(flags)))
	if errorCode != "" {
		span.SetAttributes(attribute.String(telemetry.AttrToolErrorCode, errorCode))
		span.SetStatus(codes.Error, errorCode)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	if payload, ok := telemetry.SafePayloadAttribute(telemetry.AttrPayload, envelope); ok {
		span.SetAttributes(payload)
	}
	span.End()
	if errorCode == "" {
		run.State().AddFlags(flags...)
		run.addRefs(refs)
	}
	return json.RawMessage(envelope), nil
}

// capRefs bounds the refs span attribute to the contractual 32 entries.
func capRefs(refs []string) []string {
	const maxRefs = 32
	if len(refs) > maxRefs {
		return refs[:maxRefs]
	}
	return refs
}

func (r *Runner) record(
	ctx context.Context,
	run *RunContext,
	seq int,
	name string,
	args json.RawMessage,
	envelope []byte,
	errorCode string,
	startedAt time.Time,
	flags []string,
) error {
	if r.recorder == nil {
		return fmt.Errorf("%w: no recorder configured", ErrRecordFailed)
	}
	duration := r.clock.Since(startedAt)
	if duration < 0 {
		duration = 0
	}
	durationMS := duration.Milliseconds()
	if durationMS > math.MaxInt32 {
		durationMS = math.MaxInt32
	}

	call := ToolCall{
		RunID:      run.RunID,
		Seq:        seq,
		ToolName:   name,
		Args:       args,
		Result:     envelope,
		ResultHash: hashResult(envelope),
		ErrorCode:  errorCode,
		Flags:      normalizeFlagCodes(flags),
		StartedAt:  startedAt,
		DurationMS: int(durationMS),
	}
	if err := r.recorder.RecordToolCall(ctx, call); err != nil {
		return fmt.Errorf("%w: %w", ErrRecordFailed, err)
	}
	return nil
}

func (r *Runner) maxResultBytes() int {
	if r.MaxResultBytes > 0 {
		return r.MaxResultBytes
	}
	return MaxResultBytes
}

// callHandler converts a handler panic into a generic internal error so the
// attempt is still recorded and the model never sees a stack trace.
func callHandler(ctx context.Context, tool Tool, run *RunContext, args json.RawMessage) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = ErrInternal
		}
	}()
	return tool.Call(ctx, run, args)
}

// Execer is the write surface required by DBRecorder. It is satisfied by
// *pgxpool.Pool and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// DBRecorder writes tool_calls rows through the recon_app pool. It is
// deliberately separate from Queryer so handlers can never receive it.
type DBRecorder struct {
	execer Execer
}

// NewDBRecorder returns a recorder backed by the application pool.
func NewDBRecorder(execer Execer) *DBRecorder {
	return &DBRecorder{execer: execer}
}

// RecordToolCall inserts one tool_calls row. Args and Result are passed as
// strings to explicit ::jsonb casts; passing []byte would make pgx encode
// bytea and fail the JSONB column.
func (r *DBRecorder) RecordToolCall(ctx context.Context, call ToolCall) error {
	if r == nil || r.execer == nil {
		return errors.New("tools: recorder has no database pool")
	}
	args := call.Args
	if len(args) == 0 {
		args = invalidJSONArgs
	}
	result := call.Result
	if len(result) == 0 {
		result = marshalErrorEnvelope(CodeInternalError, defaultMessage(CodeInternalError))
	}

	var errorCode any
	if call.ErrorCode != "" {
		errorCode = call.ErrorCode
	}
	flags, err := json.Marshal(normalizeFlagCodes(call.Flags))
	if err != nil {
		// normalizeFlagCodes returns a []string, so json.Marshal cannot fail.
		return fmt.Errorf("tools: encode injection flags: %w", err)
	}

	_, err = r.execer.Exec(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash, error,
		      injection_flags, started_at, duration_ms)
		 VALUES ($1::uuid, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8::jsonb,
		         $9, $10)`,
		call.RunID, call.Seq, call.ToolName, string(args), string(result),
		call.ResultHash, errorCode, string(flags), call.StartedAt, call.DurationMS)
	if err != nil {
		return fmt.Errorf("tools: insert tool call: %w", err)
	}
	return nil
}
