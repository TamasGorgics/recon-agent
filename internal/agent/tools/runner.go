package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/clock"
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

	entry, known := r.registry.lookup(name)
	if !known {
		envelope := marshalErrorEnvelope(CodeUnknownTool, defaultMessage(CodeUnknownTool))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeUnknownTool, startedAt, nil)
	}
	if !validJSON {
		envelope := marshalErrorEnvelope(CodeInvalidArguments, defaultMessage(CodeInvalidArguments))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInvalidArguments, startedAt, nil)
	}
	if err := validateArguments(entry.schema, body); err != nil {
		envelope := marshalErrorEnvelope(CodeInvalidArguments, defaultMessage(CodeInvalidArguments))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInvalidArguments, startedAt, nil)
	}

	result, handlerErr := callHandler(ctx, entry.tool, run, body)
	if handlerErr != nil {
		code, message := errorPayload(handlerErr)
		envelope := marshalErrorEnvelope(code, message)
		return r.finish(ctx, run, seq, recordedName, body, envelope, code, startedAt, nil)
	}

	envelope, refs, err := buildSuccessEnvelope(result, r.maxResultBytes())
	if err != nil {
		// The handler returned data that cannot be serialized or a ref that is
		// not canonical. Do not leak the underlying error to the model.
		envelope = marshalErrorEnvelope(CodeInternalError, defaultMessage(CodeInternalError))
		return r.finish(ctx, run, seq, recordedName, body, envelope, CodeInternalError, startedAt, nil)
	}
	return r.finish(ctx, run, seq, recordedName, body, envelope, "", startedAt, refs)
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
) (json.RawMessage, error) {
	if err := r.record(ctx, run, seq, name, args, envelope, errorCode, startedAt); err != nil {
		return nil, err
	}
	if errorCode == "" {
		run.addRefs(refs)
	}
	return json.RawMessage(envelope), nil
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

	_, err := r.execer.Exec(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash, error, started_at, duration_ms)
		 VALUES ($1::uuid, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9)`,
		call.RunID, call.Seq, call.ToolName, string(args), string(result),
		call.ResultHash, errorCode, call.StartedAt, call.DurationMS)
	if err != nil {
		return fmt.Errorf("tools: insert tool call: %w", err)
	}
	return nil
}
