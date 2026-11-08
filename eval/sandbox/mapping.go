package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CaseRef identifies one scoreable scenario's eval case.
type CaseRef struct {
	ScenarioID   string
	CaseID       string
	BreakID      string // empty for synthetic cases
	Synthetic    bool
	ScenarioTime time.Time

	caseCreatedAt time.Time
}

// ScoreableScenarios returns every mapped scenario ordered by scenario id.
func (s *Sandbox) ScoreableScenarios(ctx context.Context) ([]CaseRef, error) {
	if s.Admin == nil {
		return nil, fmt.Errorf("sandbox: admin pool is closed")
	}
	rows, err := s.Admin.Query(ctx,
		`SELECT sc.scenario_id, sc.case_id::text, COALESCE(sc.break_id::text, ''),
		        sc.synthetic, sc.scenario_time
		   FROM eval_truth.scenario_cases sc
		   JOIN eval_truth.scenarios st ON st.scenario_id = sc.scenario_id
		  ORDER BY sc.scenario_id`)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list scoreable scenarios: %w", err)
	}
	defer rows.Close()

	refs := make([]CaseRef, 0)
	for rows.Next() {
		var ref CaseRef
		if err := rows.Scan(&ref.ScenarioID, &ref.CaseID, &ref.BreakID,
			&ref.Synthetic, &ref.ScenarioTime); err != nil {
			return nil, fmt.Errorf("sandbox: scan scoreable scenario: %w", err)
		}
		ref.ScenarioTime = ref.ScenarioTime.UTC()
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sandbox: iterate scoreable scenarios: %w", err)
	}
	return refs, nil
}

// CaseForScenario returns the eval case for scenarioID. Clean scenarios and
// other intentionally unscored rows return ErrNotScoreable; unknown ids return
// ErrScenarioNotFound.
func (s *Sandbox) CaseForScenario(ctx context.Context, scenarioID string) (CaseRef, error) {
	if s.Admin == nil {
		return CaseRef{}, fmt.Errorf("sandbox: admin pool is closed")
	}
	var known bool
	if err := s.Admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM eval_truth.scenarios WHERE scenario_id = $1)`,
		scenarioID).Scan(&known); err != nil {
		return CaseRef{}, fmt.Errorf("sandbox: look up scenario %q: %w", scenarioID, err)
	}
	if !known {
		return CaseRef{}, fmt.Errorf("%w: %s", ErrScenarioNotFound, scenarioID)
	}

	var ref CaseRef
	err := s.Admin.QueryRow(ctx,
		`SELECT scenario_id, case_id::text, COALESCE(break_id::text, ''),
		        synthetic, scenario_time
		   FROM eval_truth.scenario_cases
		  WHERE scenario_id = $1`, scenarioID).
		Scan(&ref.ScenarioID, &ref.CaseID, &ref.BreakID, &ref.Synthetic, &ref.ScenarioTime)
	if err == pgx.ErrNoRows {
		return CaseRef{}, fmt.Errorf("%w: %s", ErrNotScoreable, scenarioID)
	}
	if err != nil {
		return CaseRef{}, fmt.Errorf("sandbox: read scenario case %q: %w", scenarioID, err)
	}
	ref.ScenarioTime = ref.ScenarioTime.UTC()
	return ref, nil
}

// ScenarioTime returns the persisted deterministic per-scenario instant.
func (s *Sandbox) ScenarioTime(ctx context.Context, scenarioID string) (time.Time, error) {
	ref, err := s.CaseForScenario(ctx, scenarioID)
	if err != nil {
		return time.Time{}, err
	}
	return ref.ScenarioTime, nil
}
