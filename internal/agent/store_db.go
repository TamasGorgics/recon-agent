package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// RunDB is the app-pool write/read surface used by DBStore. It is satisfied by
// *pgxpool.Pool.
type RunDB interface {
	tools.Execer
	tools.Queryer
}

// DBStore is the recon_app-pool RunStore implementation.
type DBStore struct {
	db RunDB
}

// NewDBStore returns a store backed by the app pool.
func NewDBStore(db RunDB) *DBStore { return &DBStore{db: db} }

// CreateRun inserts the initial run row. Tokens, cost, and outcome keep their
// schema defaults until FinishRun.
func (s *DBStore) CreateRun(ctx context.Context, run RunRecord) error {
	if s == nil || s.db == nil {
		return errors.New("agent: run store has no database pool")
	}
	if !tools.IsUUID(run.ID) || !tools.IsUUID(run.CaseID) {
		return fmt.Errorf("agent: run and case ids must be UUIDs")
	}
	if run.Model == "" || run.PromptVersion == "" {
		return errors.New("agent: run model and prompt version are required")
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO agent_runs (id, case_id, model, prompt_version, started_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5)`,
		run.ID, run.CaseID, run.Model, run.PromptVersion, run.StartedAt)
	if err != nil {
		return fmt.Errorf("agent: insert agent run: %w", err)
	}
	return nil
}

// RecordLLMCall inserts one successful completion. Provider errors never reach
// this method because llm_calls has no error column.
func (s *DBStore) RecordLLMCall(ctx context.Context, call LLMCall) error {
	if s == nil || s.db == nil {
		return errors.New("agent: run store has no database pool")
	}
	if !tools.IsUUID(call.RunID) {
		return errors.New("agent: llm call run id must be a UUID")
	}
	if call.Seq <= 0 || call.Model == "" || call.RequestHash == "" {
		return errors.New("agent: llm call seq, model, and request hash are required")
	}
	if len(call.Response) == 0 || len(call.Usage) == 0 {
		return errors.New("agent: llm call response and usage are required")
	}
	if call.DurationMS < 0 {
		call.DurationMS = 0
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO llm_calls
		     (agent_run_id, seq, model, request_hash, response, usage,
		      started_at, duration_ms)
		 VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8)`,
		call.RunID, call.Seq, call.Model, call.RequestHash,
		string(call.Response), string(call.Usage), call.StartedAt, call.DurationMS)
	if err != nil {
		return fmt.Errorf("agent: insert llm call: %w", err)
	}
	return nil
}

// FinishRun writes the single final run update. ended_at is never before
// started_at by construction in the loop; the schema enforces it.
func (s *DBStore) FinishRun(ctx context.Context, finish RunFinish) error {
	if s == nil || s.db == nil {
		return errors.New("agent: run store has no database pool")
	}
	if !finish.Outcome.Valid() {
		return fmt.Errorf("agent: invalid outcome %q", finish.Outcome)
	}
	if finish.TokensIn < 0 || finish.TokensOut < 0 {
		return errors.New("agent: token counts must be non-negative")
	}
	if finish.CostUSD.IsNegative() {
		return errors.New("agent: cost must be non-negative")
	}
	flags, err := json.Marshal(normalizedFlags(finish.Flags))
	if err != nil {
		return fmt.Errorf("agent: encode injection flags: %w", err)
	}
	tag, err := s.db.Exec(ctx,
		`UPDATE agent_runs
		    SET ended_at = $2,
		        tokens_in = $3,
		        tokens_out = $4,
		        cost_usd = $5::numeric,
		        outcome = $6,
		        injection_flags = $7::jsonb,
		        model = $8
		  WHERE id = $1::uuid`,
		finish.RunID, finish.EndedAt, finish.TokensIn, finish.TokensOut,
		finish.CostUSD.String(), string(finish.Outcome), string(flags),
		finish.Model)
	if err != nil {
		return fmt.Errorf("agent: finalize agent run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrRunNotFound, finish.RunID)
	}
	return nil
}

// ProposalForRun reads the one proposal row a run recorded. It is called only
// for OutcomeProposal.
func (s *DBStore) ProposalForRun(ctx context.Context, runID string) (ProposalRef, error) {
	if s == nil || s.db == nil {
		return ProposalRef{}, errors.New("agent: run store has no database pool")
	}
	var ref ProposalRef
	err := s.db.QueryRow(ctx,
		`SELECT id::text, type, status FROM proposals
		  WHERE agent_run_id = $1::uuid
		  ORDER BY created_at DESC
		  LIMIT 1`, runID).Scan(&ref.ID, &ref.Type, &ref.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProposalRef{}, fmt.Errorf("%w: %s", ErrProposalNotFound, runID)
	}
	if err != nil {
		return ProposalRef{}, fmt.Errorf("agent: read proposal for run: %w", err)
	}
	return ref, nil
}

// clampDuration converts a clock duration to non-negative milliseconds.
func clampDuration(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int(duration.Milliseconds())
}
