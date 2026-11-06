package agentrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
)

type reportQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// fillReport adds the proposal status/rationale, the safe tool-step list, and
// the latest assistant text. It never reads or prints raw tool arguments or
// results.
func fillReport(ctx context.Context, q reportQueryer, report *Report) error {
	if report.RunID == "" {
		return nil
	}
	rows, err := q.Query(ctx,
		`SELECT seq, tool_name, COALESCE(error, ''), duration_ms
		   FROM tool_calls
		  WHERE agent_run_id = $1::uuid
		  ORDER BY seq`, report.RunID)
	if err != nil {
		return fmt.Errorf("agent: read tool transcript: %w", err)
	}
	steps := make([]Step, 0)
	for rows.Next() {
		var step Step
		if err := rows.Scan(&step.Seq, &step.Tool, &step.ErrorCode, &step.DurationMS); err != nil {
			rows.Close()
			return fmt.Errorf("agent: scan tool transcript: %w", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("agent: read tool transcript: %w", err)
	}
	rows.Close()
	report.ToolSteps = steps

	var assistant string
	err = q.QueryRow(ctx,
		`SELECT COALESCE(response->'message'->>'content', '')
		   FROM llm_calls
		  WHERE agent_run_id = $1::uuid
		    AND COALESCE(response->'message'->>'content', '') <> ''
		  ORDER BY seq DESC
		  LIMIT 1`, report.RunID).Scan(&assistant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("agent: read assistant text: %w", err)
	}
	report.Assistant = truncateRunes(untrusted.Sanitize(assistant), 200)

	var (
		proposalID     string
		proposalType   string
		proposalStatus string
		rationale      string
	)
	err = q.QueryRow(ctx,
		`SELECT id::text, type, status, rationale
		   FROM proposals
		  WHERE agent_run_id = $1::uuid
		  ORDER BY created_at DESC
		  LIMIT 1`, report.RunID).Scan(&proposalID, &proposalType, &proposalStatus, &rationale)
	switch {
	case err == nil:
		report.ProposalID = proposalID
		report.ProposalType = proposalType
		report.ProposalStatus = proposalStatus
		if report.Assistant == "" {
			report.Assistant = truncateRunes(untrusted.Sanitize(rationale), 200)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// Escalated or failed run: no proposal row.
	default:
		return fmt.Errorf("agent: read proposal: %w", err)
	}
	return nil
}

func truncateRunes(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

// ValidateMode is exported for the command flag parser so an unknown --llm is
// a usage error before config loading.
func ValidateMode(mode string) error {
	switch Mode(strings.TrimSpace(mode)) {
	case ModeScripted, ModeOpencode:
		return nil
	default:
		return fmt.Errorf("agent: --llm must be scripted or opencode, got %q", mode)
	}
}

// ValidateCaseID is exported for the command flag parser.
func ValidateCaseID(caseID string) error {
	if !tools.IsUUID(strings.ToLower(strings.TrimSpace(caseID))) {
		return fmt.Errorf("agent: --case must be a UUID, got %q", caseID)
	}
	return nil
}
