package proposal

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// EvidenceReader returns the canonical refs from successful tool calls in one
// run. The production implementation reads the app pool; tests inject a fake.
type EvidenceReader interface {
	RunRefs(ctx context.Context, runID string) ([]string, error)
}

// DBEvidenceReader reads tool_calls.result.refs through the recon_app pool.
// Only successful rows (error IS NULL) count: a truncated or failed result
// cannot authorize a citation.
type DBEvidenceReader struct {
	Queryer tools.Queryer
}

// RunRefs implements EvidenceReader.
func (r *DBEvidenceReader) RunRefs(ctx context.Context, runID string) ([]string, error) {
	if r == nil || r.Queryer == nil {
		return nil, fmt.Errorf("proposal: evidence reader has no queryer")
	}
	rows, err := r.Queryer.Query(ctx,
		`SELECT DISTINCT ref
		   FROM tool_calls,
		        jsonb_array_elements_text(result->'refs') AS ref
		  WHERE agent_run_id = $1::uuid
		    AND error IS NULL
		    AND jsonb_typeof(result->'refs') = 'array'`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("proposal: read run evidence refs: %w", err)
	}
	return tools.CollectRows(rows, func(row pgx.Rows) (string, error) {
		var ref string
		if err := row.Scan(&ref); err != nil {
			return "", err
		}
		return ref, nil
	})
}

// validateEvidence runs the ordered evidence checks: canonical format, the
// in-memory RetrievedRefs precheck, then the authoritative app-pool set. It
// returns the sorted unique refs used by the idempotency key and state hash.
func validateEvidence(ctx context.Context, run *tools.RunContext, refs []string, reader EvidenceReader) ([]string, error) {
	if len(refs) == 0 {
		return nil, invalidEvidenceError()
	}
	for _, ref := range refs {
		if err := tools.ValidateRef(ref); err != nil {
			return nil, invalidEvidenceError()
		}
	}
	canonical := tools.SortedUnique(refs)

	inMemory := make(map[string]struct{})
	for _, ref := range run.RetrievedRefs() {
		inMemory[ref] = struct{}{}
	}
	for _, ref := range canonical {
		if _, ok := inMemory[ref]; !ok {
			return nil, evidenceNotRetrievedError()
		}
	}

	if reader == nil {
		return nil, fmt.Errorf("proposal: no evidence reader configured")
	}
	stored, err := reader.RunRefs(ctx, run.RunID)
	if err != nil {
		return nil, fmt.Errorf("proposal: read evidence: %w", err)
	}
	authoritative := make(map[string]struct{}, len(stored))
	for _, ref := range stored {
		authoritative[ref] = struct{}{}
	}
	for _, ref := range canonical {
		if _, ok := authoritative[ref]; !ok {
			return nil, evidenceNotRetrievedError()
		}
	}
	return canonical, nil
}
