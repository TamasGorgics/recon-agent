package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SaveRunFlags persists the run's injection-flag codes on agent_runs through
// the app pool. Empty flags are stored as [] (never NULL); the list is
// de-duplicated and sorted, and a repeated write is idempotent. M2-09 calls
// this at run end; M2-07 has no run lifecycle of its own.
func SaveRunFlags(ctx context.Context, execer Execer, runID string, flags []string) error {
	if execer == nil {
		return errors.New("tools: save run flags: no database pool")
	}
	if runID == "" {
		return fmt.Errorf("%w: run id is required", ErrInvalidRunContext)
	}
	payload, err := json.Marshal(normalizeFlagCodes(flags))
	if err != nil {
		return fmt.Errorf("tools: save run flags: %w", err)
	}
	tag, err := execer.Exec(ctx,
		`UPDATE agent_runs SET injection_flags = $2::jsonb WHERE id = $1::uuid`,
		runID, string(payload))
	if err != nil {
		return fmt.Errorf("tools: save run flags: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("tools: save run flags: run %s not found", runID)
	}
	return nil
}
