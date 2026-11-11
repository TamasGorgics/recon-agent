package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Switch action and entity names written to audit_log.
const (
	ActionSwitchSet = "switch.set"
	EntitySwitchSet = "system_switch"
	SwitchAgent     = "agent_enabled"
	SwitchAutoApply = "auto_apply_enabled"
)

var errSwitchMissing = errors.New("api: seeded system switch is missing")

// SwitchWriter updates a seeded switch and appends its audit entry in one
// recon_app transaction.
type SwitchWriter struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewSwitchWriter builds the switch write service.
func NewSwitchWriter(pool *pgxpool.Pool, clk clock.Clock) *SwitchWriter {
	return &SwitchWriter{pool: pool, clock: clk}
}

// SetSwitch implements the write half of POST /v1/admin/switches.
func (s *SwitchWriter) SetSwitch(ctx context.Context, req SwitchSetRequest) (SwitchResult, error) {
	if req.Name != SwitchAgent && req.Name != SwitchAutoApply {
		return SwitchResult{}, fmt.Errorf("%w: unknown switch name", cases.ErrInvalidRequest)
	}
	actor, err := audit.HumanActor(req.ActorID)
	if err != nil {
		return SwitchResult{}, err
	}
	updatedBy := actor.String()

	var result SwitchResult
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// Reset on every attempt: WithTx may re-run the callback after a
		// serialization failure or deadlock.
		result = SwitchResult{Name: req.Name}

		var (
			enabled   bool
			reason    string
			oldActor  string
			updatedAt time.Time
		)
		err := tx.QueryRow(ctx,
			`SELECT enabled, reason, updated_by, updated_at
			   FROM system_switches
			  WHERE name = $1
			  FOR UPDATE`, req.Name).
			Scan(&enabled, &reason, &oldActor, &updatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errSwitchMissing
		}
		if err != nil {
			return fmt.Errorf("api: read switch: %w", err)
		}
		updatedAt = updatedAt.UTC()

		if enabled == req.Enabled {
			result.Enabled = enabled
			result.Changed = false
			result.UpdatedBy = oldActor
			result.UpdatedAt = updatedAt
			return nil
		}

		now := s.clock.Now().UTC().Truncate(time.Microsecond)
		if _, err := tx.Exec(ctx,
			`UPDATE system_switches
			    SET enabled = $2, reason = $3, updated_by = $4, updated_at = $5
			  WHERE name = $1`, req.Name, req.Enabled, req.Reason, updatedBy, now); err != nil {
			return fmt.Errorf("api: update switch: %w", err)
		}

		var traceID *string
		if req.TraceID != "" {
			trace := req.TraceID
			traceID = &trace
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      actor,
			Action:     ActionSwitchSet,
			EntityType: EntitySwitchSet,
			EntityID:   req.Name,
			Before: map[string]any{
				"enabled": enabled,
				"reason":  reason,
			},
			After: map[string]any{
				"enabled": req.Enabled,
				"reason":  req.Reason,
			},
			TraceID:   traceID,
			Timestamp: now,
		}); err != nil {
			return fmt.Errorf("api: append switch audit: %w", err)
		}

		result.Enabled = req.Enabled
		result.Changed = true
		result.UpdatedBy = updatedBy
		result.UpdatedAt = now
		return nil
	})
	if err != nil {
		return SwitchResult{}, err
	}
	return result, nil
}

// Ensure SwitchWriter satisfies the handler interface.
var _ SwitchService = (*SwitchWriter)(nil)
