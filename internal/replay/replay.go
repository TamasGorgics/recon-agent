// Package replay reconstructs a recorded agent run without executing tools or
// touching live data. LoadRecording is the only function in this package that
// reads a database; ExactReplay takes a fully loaded Recording and has no
// database, context, pool, transaction, or client parameter.
package replay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// ModeExact is the exact replay mode reported by Report.Mode.
const ModeExact = "exact"

// Sentinel errors. The CLI maps these to exit code 2.
var (
	// ErrInvalidRunID means the supplied run id is not a canonical UUID.
	ErrInvalidRunID = errors.New("replay: run id is not a canonical UUID")
	// ErrRunNotFound means agent_runs has no row for the supplied run id.
	ErrRunNotFound = errors.New("replay: run not found")
	// ErrUnsupportedRun means exact mode cannot replay the run's outcome.
	ErrUnsupportedRun = errors.New("replay: unsupported run")
	// ErrInvalidRecording means the loaded rows cannot form a replayable step
	// sequence. It is distinct from a divergence: the recording is malformed.
	ErrInvalidRecording = errors.New("replay: invalid recording")
	// ErrRebuild means the recorded proposal arguments cannot be normalized
	// under the current code.
	ErrRebuild = errors.New("replay: proposal rebuild failed")
)

// UnsupportedRunError names the actual outcome and mode so the CLI can print
// it. It unwraps to ErrUnsupportedRun. Mode is "exact" when empty.
type UnsupportedRunError struct {
	RunID       string
	Outcome     string
	HasProposal bool
	Mode        string
}

func (e *UnsupportedRunError) Error() string {
	outcome := strings.TrimSpace(e.Outcome)
	if outcome == "" {
		outcome = "none"
	}
	mode := strings.TrimSpace(e.Mode)
	if mode == "" {
		mode = ModeExact
	}
	if e.Outcome == "proposal" && !e.HasProposal {
		return fmt.Sprintf("%s: run %s outcome proposal has no stored proposal (%s mode requires one)",
			ErrUnsupportedRun, e.RunID, mode)
	}
	return fmt.Sprintf("%s: run %s outcome %q (%s mode requires outcome proposal with a stored proposal)",
		ErrUnsupportedRun, e.RunID, outcome, mode)
}

// Unwrap returns ErrUnsupportedRun.
func (e *UnsupportedRunError) Unwrap() error { return ErrUnsupportedRun }

// Run is the loaded agent_runs row.
type Run struct {
	ID            string
	CaseID        string
	Model         string
	PromptVersion string
	Outcome       string
	StartedAt     time.Time
	EndedAt       *time.Time
}

// LLMCall is one recorded successful completion. Response and Usage are the
// stored normalized JSON values (JSONB text at load time).
type LLMCall struct {
	Seq         int
	Model       string
	RequestHash string
	Response    []byte
	Usage       []byte
	StartedAt   time.Time
	DurationMS  int
}

// ToolCall is one recorded attempted tool call. Result is the JSONB text of the
// stored envelope, and ResultCanonicalHash is NULL only for legacy rows.
type ToolCall struct {
	Seq                 int
	ToolName            string
	Args                []byte
	Result              []byte
	ResultHash          string
	ResultCanonicalHash *string
	Error               *string
	InjectionFlags      []byte
	StartedAt           time.Time
	DurationMS          int
}

// Proposal is the latest stored proposals row for the run. Payload and
// Evidence are JSONB text; Confidence is the NUMERIC(5,4) text form.
type Proposal struct {
	ID             string
	Type           string
	Payload        []byte
	Rationale      string
	Evidence       []byte
	Confidence     string
	PatternClaimed *string
	StateHash      string
	IdempotencyKey string
}

// Recording is the complete in-memory run snapshot passed to ExactReplay.
type Recording struct {
	Run       Run
	LLMCalls  []LLMCall
	ToolCalls []ToolCall
	Proposal  *Proposal
}

// LoadRecording reads one run and everything exact replay needs in a single
// read-only pass: the agent_runs row, llm_calls ordered by seq, tool_calls
// ordered by seq, and the latest proposal. It performs no writes and does not
// open a transaction; the caller owns the REPEATABLE READ READ ONLY transaction
// and closes the pool after the load.
func LoadRecording(ctx context.Context, q tools.Queryer, runID string) (Recording, error) {
	if q == nil {
		return Recording{}, fmt.Errorf("%w: no queryer", ErrInvalidRunID)
	}
	id := strings.ToLower(strings.TrimSpace(runID))
	if !tools.IsUUID(id) {
		return Recording{}, fmt.Errorf("%w: %q", ErrInvalidRunID, runID)
	}

	var (
		rec     Recording
		outcome *string
		endedAt *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, case_id::text, model, prompt_version, outcome,
		        started_at, ended_at
		   FROM agent_runs
		  WHERE id = $1::uuid`, id).
		Scan(&rec.Run.ID, &rec.Run.CaseID, &rec.Run.Model, &rec.Run.PromptVersion,
			&outcome, &rec.Run.StartedAt, &endedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Recording{}, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	if err != nil {
		return Recording{}, fmt.Errorf("replay: load run %s: %w", id, err)
	}
	rec.Run.ID = strings.ToLower(rec.Run.ID)
	rec.Run.CaseID = strings.ToLower(rec.Run.CaseID)
	if outcome != nil {
		rec.Run.Outcome = *outcome
	}
	rec.Run.EndedAt = endedAt

	rec.LLMCalls, err = loadLLMCalls(ctx, q, id)
	if err != nil {
		return Recording{}, err
	}
	rec.ToolCalls, err = loadToolCalls(ctx, q, id)
	if err != nil {
		return Recording{}, err
	}
	rec.Proposal, err = loadProposal(ctx, q, id)
	if err != nil {
		return Recording{}, err
	}
	return rec, nil
}

func loadLLMCalls(ctx context.Context, q tools.Queryer, runID string) ([]LLMCall, error) {
	rows, err := q.Query(ctx,
		`SELECT seq, model, request_hash, response::text, usage::text,
		        started_at, duration_ms
		   FROM llm_calls
		  WHERE agent_run_id = $1::uuid
		  ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("replay: load llm calls: %w", err)
	}
	defer rows.Close()

	calls := make([]LLMCall, 0)
	for rows.Next() {
		var (
			call     LLMCall
			response string
			usage    string
		)
		if err := rows.Scan(&call.Seq, &call.Model, &call.RequestHash, &response,
			&usage, &call.StartedAt, &call.DurationMS); err != nil {
			return nil, fmt.Errorf("replay: scan llm call: %w", err)
		}
		call.Response = []byte(response)
		call.Usage = []byte(usage)
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replay: read llm calls: %w", err)
	}
	return calls, nil
}

func loadToolCalls(ctx context.Context, q tools.Queryer, runID string) ([]ToolCall, error) {
	rows, err := q.Query(ctx,
		`SELECT seq, tool_name, args::text, result::text, result_hash,
		        result_canonical_hash, error, injection_flags::text,
		        started_at, duration_ms
		   FROM tool_calls
		  WHERE agent_run_id = $1::uuid
		  ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("replay: load tool calls: %w", err)
	}
	defer rows.Close()

	calls := make([]ToolCall, 0)
	for rows.Next() {
		var (
			call      ToolCall
			args      string
			result    string
			flags     string
			canonical *string
			errorCode *string
		)
		if err := rows.Scan(&call.Seq, &call.ToolName, &args, &result,
			&call.ResultHash, &canonical, &errorCode, &flags,
			&call.StartedAt, &call.DurationMS); err != nil {
			return nil, fmt.Errorf("replay: scan tool call: %w", err)
		}
		call.Args = []byte(args)
		call.Result = []byte(result)
		call.ResultCanonicalHash = canonical
		call.Error = errorCode
		call.InjectionFlags = []byte(flags)
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replay: read tool calls: %w", err)
	}
	return calls, nil
}

func loadProposal(ctx context.Context, q tools.Queryer, runID string) (*Proposal, error) {
	var (
		proposal Proposal
		payload  string
		evidence string
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, type, payload::text, rationale, evidence::text,
		        confidence::text, pattern_claimed, state_hash, idempotency_key
		   FROM proposals
		  WHERE agent_run_id = $1::uuid
		  ORDER BY created_at DESC
		  LIMIT 1`, runID).
		Scan(&proposal.ID, &proposal.Type, &payload, &proposal.Rationale,
			&evidence, &proposal.Confidence, &proposal.PatternClaimed,
			&proposal.StateHash, &proposal.IdempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("replay: load proposal: %w", err)
	}
	proposal.ID = strings.ToLower(proposal.ID)
	proposal.Payload = []byte(payload)
	proposal.Evidence = []byte(evidence)
	return &proposal, nil
}
