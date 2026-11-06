package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// Proposal is one row to insert. The agent role cannot read proposal columns,
// so the UUID is client-side and the insert never RETURNINGs.
type Proposal struct {
	ID             string
	CaseID         string
	RunID          string
	Type           string
	Payload        map[string]any
	Rationale      string
	Evidence       []string
	Confidence     string
	Pattern        string
	StateHash      string
	IdempotencyKey string
	CreatedAt      time.Time
}

type proposalStore struct {
	execer tools.Execer
}

// insert writes one proposed row through the agent pool. A duplicate
// idempotency_key is a zero-row no-op (exact replay); a 23505 on the
// one-active-proposal partial index maps to ErrActiveProposalExists, and a
// 23505 on the one-proposal-per-run index maps to ErrProposalExists. Any other
// error is returned for the caller to map to a safe tool_error.
func (s proposalStore) existsByKey(ctx context.Context, queryer tools.Queryer, key string) (bool, error) {
	if queryer == nil {
		return false, errors.New("proposal: idempotency lookup has no queryer")
	}
	var exists bool
	if err := queryer.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM proposals WHERE idempotency_key = $1)`,
		key).Scan(&exists); err != nil {
		return false, fmt.Errorf("proposal: look up idempotency key: %w", err)
	}
	return exists, nil
}

func (s proposalStore) insert(ctx context.Context, proposal Proposal) (bool, error) {
	if s.execer == nil {
		return false, errors.New("proposal: insert has no agent execer")
	}
	payload, err := json.Marshal(proposal.Payload)
	if err != nil {
		return false, fmt.Errorf("proposal: encode payload: %w", err)
	}
	evidence, err := json.Marshal(tools.SortedUnique(proposal.Evidence))
	if err != nil {
		return false, fmt.Errorf("proposal: encode evidence: %w", err)
	}
	var pattern any
	if proposal.Pattern != "" {
		pattern = proposal.Pattern
	}

	tag, err := s.execer.Exec(ctx,
		`INSERT INTO proposals
		     (id, case_id, agent_run_id, type, payload, rationale, evidence,
		      confidence, pattern_claimed, status, version, state_hash,
		      idempotency_key, created_at)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, $6, $7::jsonb,
		         $8::numeric, $9, 'proposed', 0, $10, $11, $12)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		proposal.ID, proposal.CaseID, proposal.RunID, proposal.Type,
		string(payload), proposal.Rationale, string(evidence),
		proposal.Confidence, pattern, proposal.StateHash,
		proposal.IdempotencyKey, proposal.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "proposals_one_active_per_case":
				return false, fmt.Errorf("%w: %s", ErrActiveProposalExists, pgErr.ConstraintName)
			case "proposals_one_proposal_per_run":
				return false, fmt.Errorf("%w: %s", ErrProposalExists, pgErr.ConstraintName)
			}
		}
		return false, fmt.Errorf("proposal: insert proposal: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
