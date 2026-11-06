package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// SwitchReader reads the operator kill switch. A missing agent_enabled row is
// treated as off (fail closed); a database error is returned for the caller to
// log and retry.
type SwitchReader interface {
	AgentEnabled(ctx context.Context) (bool, error)
}

// DBSwitch reads system_switches through the app pool.
type DBSwitch struct {
	Queryer tools.Queryer
}

// AgentEnabled reports whether agent_enabled is on.
func (s DBSwitch) AgentEnabled(ctx context.Context) (bool, error) {
	if s.Queryer == nil {
		return false, errors.New("agent: switch reader has no queryer")
	}
	var enabled bool
	err := s.Queryer.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = 'agent_enabled'`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("agent: read agent_enabled switch: %w", err)
	}
	return enabled, nil
}
