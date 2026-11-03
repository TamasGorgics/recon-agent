package cases

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Service creates breaks and cases and owns the case lease state machine. All
// timestamps come from the injected clock; every write joins one transaction.
type Service struct {
	db    db.Beginner
	clock clock.Clock
	actor audit.Actor
}

// NewService validates its dependencies and returns a case service.
func NewService(pool db.Beginner, clk clock.Clock, actor audit.Actor) (*Service, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	if clk == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidRequest)
	}
	if !actor.Valid() {
		return nil, fmt.Errorf("%w: invalid audit actor", ErrInvalidRequest)
	}
	return &Service{db: pool, clock: clk, actor: actor}, nil
}

// TransitionRequest is an optimistic-concurrency case status change. The caller
// passes the version it read; a mismatch is ErrStale and changes nothing.
type TransitionRequest struct {
	CaseID          string
	ExpectedVersion int
	Status          Status
	// EscalationReason is applied when non-empty.
	EscalationReason string
	// ResolutionType is applied when non-empty.
	ResolutionType string
}

const caseColumns = `id::text, priority, value_minor, currency, status, version,
	attempts, escalation_reason, created_at, leased_by, lease_expires_at,
	resolved_at, resolution_type`

type caseScanner interface {
	Scan(dest ...any) error
}

func scanCase(row caseScanner) (Case, error) {
	var (
		c          Case
		status     string
		escalation *string
		leasedBy   *string
		expires    *time.Time
		resolved   *time.Time
		resolution *string
	)
	if err := row.Scan(&c.ID, &c.Priority, &c.ValueMinor, &c.Currency, &status,
		&c.Version, &c.Attempts, &escalation, &c.CreatedAt, &leasedBy, &expires,
		&resolved, &resolution); err != nil {
		return Case{}, err
	}
	c.Status = Status(status)
	c.EscalationReason = escalation
	c.LeasedBy = leasedBy
	c.LeaseExpiresAt = expires
	c.ResolvedAt = resolved
	c.ResolutionType = resolution
	return c, nil
}

func (s *Service) loadBreakIDs(ctx context.Context, tx pgx.Tx, caseID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT break_id::text FROM case_breaks WHERE case_id = $1::uuid ORDER BY break_id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("cases: load case breaks: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("cases: scan case break: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cases: load case breaks: %w", err)
	}
	return ids, nil
}

func (s *Service) getCaseTx(ctx context.Context, tx pgx.Tx, caseID string) (Case, error) {
	c, err := scanCase(tx.QueryRow(ctx,
		`SELECT `+caseColumns+` FROM cases WHERE id = $1::uuid`, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, ErrCaseNotFound
	}
	if err != nil {
		return Case{}, fmt.Errorf("cases: get case: %w", err)
	}
	ids, err := s.loadBreakIDs(ctx, tx, caseID)
	if err != nil {
		return Case{}, err
	}
	c.BreakIDs = ids
	return c, nil
}

// Get returns one case and its break IDs.
func (s *Service) Get(ctx context.Context, caseID string) (Case, error) {
	if caseID == "" {
		return Case{}, fmt.Errorf("%w: case id is required", ErrInvalidRequest)
	}
	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		c, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		result = c
		return nil
	})
	return result, err
}

// CreateCase creates one open case from the supplied breaks. It rejects an
// empty set and a break that already belongs to a case.
func (s *Service) CreateCase(ctx context.Context, breakIDs []string) (Case, error) {
	ids := uniqueNonEmpty(breakIDs)
	if len(ids) == 0 {
		return Case{}, ErrNoBreaks
	}
	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		c, err := s.createCaseTx(ctx, tx, ids, "")
		if err != nil {
			return err
		}
		result = c
		return nil
	})
	return result, err
}

// createCaseTx validates and inserts one case plus its case_breaks membership.
// The caller owns the transaction; the audit entries join it.
func (s *Service) createCaseTx(ctx context.Context, tx pgx.Tx, breakIDs []string, group string) (Case, error) {
	ids := uniqueNonEmpty(breakIDs)
	if len(ids) == 0 {
		return Case{}, ErrNoBreaks
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)

	breaks, err := s.loadBreaksForUpdate(ctx, tx, ids)
	if err != nil {
		return Case{}, err
	}
	if len(breaks) != len(ids) {
		return Case{}, ErrBreakNotFound
	}
	var assigned int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM case_breaks WHERE break_id::text = ANY($1::text[])`, ids).
		Scan(&assigned); err != nil {
		return Case{}, fmt.Errorf("cases: check break assignment: %w", err)
	}
	if assigned > 0 {
		return Case{}, ErrBreakInCase
	}

	currency := breaks[0].Currency
	var value int64
	for _, b := range breaks {
		if b.Status == BreakStatusInCase {
			return Case{}, ErrBreakInCase
		}
		if b.Status != BreakStatusOpen {
			return Case{}, fmt.Errorf("%w: %s is %s", ErrBreakNotOpen, b.ID, b.Status)
		}
		if b.Currency != currency {
			return Case{}, fmt.Errorf("%w: breaks use %s and %s", ErrInvalidRequest, currency, b.Currency)
		}
		amount := absMinor(b.DeltaMinor)
		if value > math.MaxInt64-amount {
			return Case{}, fmt.Errorf("%w: case value overflows int64", ErrInvalidRequest)
		}
		value += amount
	}

	var caseID string
	err = tx.QueryRow(ctx,
		`INSERT INTO cases (priority, value_minor, currency, status, version, attempts, created_at)
		 VALUES ($1, $2, $3, 'open', 0, 0, $4) RETURNING id::text`,
		PriorityForValue(value), value, currency, now).Scan(&caseID)
	if err != nil {
		return Case{}, fmt.Errorf("cases: insert case: %w", err)
	}

	for _, b := range breaks {
		if _, err := tx.Exec(ctx,
			`INSERT INTO case_breaks (case_id, break_id, created_at)
			 VALUES ($1::uuid, $2::uuid, $3)`, caseID, b.ID, now); err != nil {
			return Case{}, fmt.Errorf("cases: link break %s: %w", b.ID, err)
		}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE breaks SET status = 'in_case'
		 WHERE id::text = ANY($1::text[]) AND status = 'open'`, ids)
	if err != nil {
		return Case{}, fmt.Errorf("cases: mark breaks in case: %w", err)
	}
	if tag.RowsAffected() != int64(len(breaks)) {
		return Case{}, ErrBreakInCase
	}

	for _, b := range breaks {
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionBreakInCase,
			EntityType: EntityBreak,
			EntityID:   b.ID,
			Before:     map[string]any{"status": string(b.Status)},
			After:      map[string]any{"status": string(BreakStatusInCase), "case_id": caseID},
			Timestamp:  now,
		}); err != nil {
			return Case{}, err
		}
	}
	after := map[string]any{
		"priority":    PriorityForValue(value),
		"value_minor": value,
		"currency":    currency,
		"status":      string(StatusOpen),
		"version":     0,
		"break_ids":   ids,
	}
	if group != "" {
		after["group"] = group
	}
	if _, err := audit.Append(ctx, tx, audit.Entry{
		Actor:      s.actor,
		Action:     ActionCaseCreate,
		EntityType: EntityCase,
		EntityID:   caseID,
		After:      after,
		Timestamp:  now,
	}); err != nil {
		return Case{}, err
	}
	return s.getCaseTx(ctx, tx, caseID)
}

// Transition moves one case to a new status with optimistic concurrency. The
// update is guarded by both the row lock and WHERE version = expected; a stale
// caller gets ErrStale and no audit row is written.
func (s *Service) Transition(ctx context.Context, req TransitionRequest) (Case, error) {
	if req.CaseID == "" {
		return Case{}, fmt.Errorf("%w: case id is required", ErrInvalidRequest)
	}
	if req.ExpectedVersion < 0 {
		return Case{}, fmt.Errorf("%w: expected version must be non-negative", ErrInvalidRequest)
	}
	if !req.Status.Valid() {
		return Case{}, fmt.Errorf("%w: %q", ErrInvalidStatus, req.Status)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		before, err := scanCase(tx.QueryRow(ctx,
			`SELECT `+caseColumns+` FROM cases WHERE id = $1::uuid FOR UPDATE`, req.CaseID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCaseNotFound
		}
		if err != nil {
			return fmt.Errorf("cases: lock case: %w", err)
		}
		if before.Version != req.ExpectedVersion {
			return fmt.Errorf("%w: have %d, caller expected %d",
				ErrStale, before.Version, req.ExpectedVersion)
		}

		tag, err := tx.Exec(ctx,
			`UPDATE cases
			    SET status = $2,
			        version = version + 1,
			        leased_by = CASE WHEN $2 = 'in_agent' THEN leased_by ELSE NULL END,
			        lease_expires_at = CASE WHEN $2 = 'in_agent' THEN lease_expires_at ELSE NULL END,
			        escalation_reason = COALESCE(NULLIF($3, ''), escalation_reason),
			        resolved_at = CASE WHEN $2 = 'resolved'
			                           THEN COALESCE(resolved_at, $4) ELSE resolved_at END,
			        resolution_type = COALESCE(NULLIF($5, ''), resolution_type)
			  WHERE id = $1::uuid AND version = $6`,
			req.CaseID, string(req.Status), req.EscalationReason, now,
			req.ResolutionType, req.ExpectedVersion)
		if err != nil {
			return fmt.Errorf("cases: transition case: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrStale
		}

		afterPayload := map[string]any{
			"status":  string(req.Status),
			"version": before.Version + 1,
		}
		if req.EscalationReason != "" {
			afterPayload["escalation_reason"] = req.EscalationReason
		}
		if req.ResolutionType != "" {
			afterPayload["resolution_type"] = req.ResolutionType
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseTransition,
			EntityType: EntityCase,
			EntityID:   req.CaseID,
			Before:     map[string]any{"status": string(before.Status), "version": before.Version},
			After:      afterPayload,
			Timestamp:  now,
		}); err != nil {
			return err
		}
		c, err := s.getCaseTx(ctx, tx, req.CaseID)
		if err != nil {
			return err
		}
		result = c
		return nil
	})
	return result, err
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
