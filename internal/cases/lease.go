package cases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	casestate "github.com/TamasGorgics/recon-agent/internal/cases/state"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

// Lease claims the highest-priority open case for workerID with an expiring
// lease. The selection uses FOR UPDATE SKIP LOCKED, so concurrent workers never
// see the same row. The case leaves the open pool and version increments by one.
// Expired leases are reclaimed inline before selection.
func (s *Service) Lease(ctx context.Context, workerID string, ttl time.Duration) (Case, error) {
	if strings.TrimSpace(workerID) == "" {
		return Case{}, fmt.Errorf("%w: worker id is required", ErrInvalidRequest)
	}
	if ttl <= 0 {
		return Case{}, fmt.Errorf("%w: got %s", ErrInvalidTTL, ttl)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(ttl)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.reclaimExpiredTx(ctx, tx, now); err != nil {
			return err
		}

		var (
			caseID  string
			version int
		)
		err := tx.QueryRow(ctx,
			`SELECT id::text, version
			   FROM cases
			  WHERE status = 'open' AND leased_by IS NULL
			  ORDER BY priority DESC, created_at ASC, id ASC
			  LIMIT 1
			  FOR UPDATE SKIP LOCKED`).Scan(&caseID, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoCase
		}
		if err != nil {
			return fmt.Errorf("cases: select lease candidate: %w", err)
		}

		if _, err := s.transitionStatus(ctx, tx, caseID, StatusInAgent, version, "", now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE cases
			    SET leased_by = $1,
			        lease_expires_at = $2
			  WHERE id = $3::uuid AND status = 'in_agent' AND leased_by IS NULL`,
			workerID, expires, caseID)
		if err != nil {
			return fmt.Errorf("cases: attach lease to %s: %w", caseID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: case %s changed while leasing", ErrStale, caseID)
		}

		leased, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseLease,
			EntityType: EntityCase,
			EntityID:   caseID,
			Before: map[string]any{
				"status":  string(StatusOpen),
				"version": leased.Version - 1,
			},
			After: map[string]any{
				"status":           string(StatusInAgent),
				"version":          leased.Version,
				"attempts":         leased.Attempts,
				"worker_id":        workerID,
				"lease_expires_at": expires,
			},
			Timestamp: now,
		}); err != nil {
			return err
		}
		result = leased
		return nil
	})
	return result, err
}

// LeaseByID claims one named open case for workerID. It is the CLI/gate path:
// unlike Lease, it never scans the queue, so running a specific fixture case
// cannot acquire and strand an unrelated open case. The owner guard and audit
// entry are identical to Lease.
func (s *Service) LeaseByID(ctx context.Context, caseID, workerID string, ttl time.Duration) (Case, error) {
	if caseID == "" || strings.TrimSpace(workerID) == "" {
		return Case{}, fmt.Errorf("%w: case and worker ids are required", ErrInvalidRequest)
	}
	if ttl <= 0 {
		return Case{}, fmt.Errorf("%w: got %s", ErrInvalidTTL, ttl)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(ttl)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		state, err := s.lockLease(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if state.status != StatusOpen || state.leasedBy != nil {
			return fmt.Errorf("%w: case is %s", ErrNotLeased, state.status)
		}
		if _, err := s.transitionStatus(ctx, tx, caseID, StatusInAgent, state.version, "", now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE cases
			    SET leased_by = $2,
			        lease_expires_at = $3
			  WHERE id = $1::uuid AND status = 'in_agent' AND leased_by IS NULL`,
			caseID, workerID, expires)
		if err != nil {
			return fmt.Errorf("cases: lease case %s: %w", caseID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrStale
		}
		leased, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseLease,
			EntityType: EntityCase,
			EntityID:   caseID,
			Before: map[string]any{
				"status":  string(StatusOpen),
				"version": leased.Version - 1,
			},
			After: map[string]any{
				"status":           string(StatusInAgent),
				"version":          leased.Version,
				"attempts":         leased.Attempts,
				"worker_id":        workerID,
				"lease_expires_at": expires,
			},
			Timestamp: now,
		}); err != nil {
			return err
		}
		result = leased
		return nil
	})
	return result, err
}

// Renew extends the owner's live lease. A non-owner, an unleased case, or an
// expired lease is rejected; only the owner and only before expiry.
func (s *Service) Renew(ctx context.Context, caseID, workerID string, ttl time.Duration) (Case, error) {
	if caseID == "" || strings.TrimSpace(workerID) == "" {
		return Case{}, fmt.Errorf("%w: case and worker ids are required", ErrInvalidRequest)
	}
	if ttl <= 0 {
		return Case{}, fmt.Errorf("%w: got %s", ErrInvalidTTL, ttl)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(ttl)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		state, err := s.lockLease(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if err := state.checkOwner(workerID); err != nil {
			return err
		}
		if !state.expires.After(now) {
			return ErrLeaseExpired
		}
		tag, err := tx.Exec(ctx,
			`UPDATE cases SET lease_expires_at = $3, version = version + 1
			  WHERE id = $1::uuid AND leased_by = $2 AND status = 'in_agent'
			    AND lease_expires_at > $4`,
			caseID, workerID, expires, now)
		if err != nil {
			return fmt.Errorf("cases: renew lease: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseExpired
		}
		renewed, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseRenew,
			EntityType: EntityCase,
			EntityID:   caseID,
			Before:     map[string]any{"lease_expires_at": state.expires, "version": state.version},
			After: map[string]any{
				"lease_expires_at": expires,
				"version":          renewed.Version,
				"worker_id":        workerID,
			},
			Timestamp: now,
		}); err != nil {
			return err
		}
		result = renewed
		return nil
	})
	return result, err
}

// Release returns the owner's lease to the open pool without incrementing
// attempts. Releasing twice or by a non-owner is an error.
func (s *Service) Release(ctx context.Context, caseID, workerID string) (Case, error) {
	if caseID == "" || strings.TrimSpace(workerID) == "" {
		return Case{}, fmt.Errorf("%w: case and worker ids are required", ErrInvalidRequest)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		state, err := s.lockLease(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if err := state.checkOwner(workerID); err != nil {
			return err
		}
		if _, err := s.transitionStatus(ctx, tx, caseID, StatusOpen, state.version, "", now); err != nil {
			return err
		}
		released, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseRelease,
			EntityType: EntityCase,
			EntityID:   caseID,
			Before:     map[string]any{"lease_expires_at": state.expires, "version": state.version},
			After: map[string]any{
				"status":    string(StatusOpen),
				"version":   released.Version,
				"worker_id": workerID,
			},
			Timestamp: now,
		}); err != nil {
			return err
		}
		result = released
		return nil
	})
	return result, err
}

// RecordFailedAttempt atomically returns a failed run's lease to the queue and
// increments attempts. It is the counted path for budget_exhausted and
// invalid_output outcomes; Release remains the no-attempt path. When the new
// attempt count reaches maxAttempts the case is escalated with reason
// attempts_exhausted instead of returning to open.
//
// The case row is locked and both status='in_agent' and leased_by=workerID are
// re-checked inside the transaction, so a worker whose lease was reclaimed
// cannot double-increment or overwrite another worker's case. A lost lease
// returns ErrNotLeaseOwner/ErrNotLeased/ErrStale and changes nothing.
func (s *Service) RecordFailedAttempt(ctx context.Context, caseID, workerID string, maxAttempts int) (Case, error) {
	if caseID == "" || strings.TrimSpace(workerID) == "" {
		return Case{}, fmt.Errorf("%w: case and worker ids are required", ErrInvalidRequest)
	}
	if maxAttempts < 1 {
		return Case{}, fmt.Errorf("%w: max attempts must be positive", ErrInvalidRequest)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)

	var result Case
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		state, err := s.lockLease(ctx, tx, caseID)
		if err != nil {
			return err
		}
		if err := state.checkOwner(workerID); err != nil {
			return err
		}

		attempts := state.attempts + 1
		status := StatusOpen
		reason := ""
		if attempts >= maxAttempts {
			status = StatusEscalated
			reason = casestate.EscalationAttemptsExhausted
		}

		snapshot, err := s.transitionStatus(ctx, tx, caseID, status, state.version, reason, now)
		if err != nil {
			return err
		}
		newAttempts, err := incrementAttempts(ctx, tx, caseID, snapshot.Version)
		if err != nil {
			return err
		}

		after := map[string]any{
			"status":    string(status),
			"version":   snapshot.Version,
			"attempts":  newAttempts,
			"worker_id": workerID,
		}
		if reason != "" {
			after["escalation_reason"] = reason
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseAttempt,
			EntityType: EntityCase,
			EntityID:   caseID,
			Before: map[string]any{
				"status":    string(state.status),
				"version":   state.version,
				"attempts":  state.attempts,
				"worker_id": workerID,
			},
			After:     after,
			Timestamp: now,
		}); err != nil {
			return err
		}
		updated, err := s.getCaseTx(ctx, tx, caseID)
		if err != nil {
			return err
		}
		result = updated
		return nil
	})
	return result, err
}

// ReclaimExpired returns every expired lease to the open pool, increments
// attempts once per reclaimed lease, and audits each reclaim. When the new
// attempt count reaches the service attempt cap the case is escalated with
// reason attempts_exhausted instead of returning to open, so it cannot be leased
// again. Lease calls the same path inline, so callers can also run it on a
// schedule.
func (s *Service) ReclaimExpired(ctx context.Context) (int, error) {
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	var count int
	err := db.WithTx(ctx, s.db, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		n, err := s.reclaimExpiredTx(ctx, tx, now)
		if err != nil {
			return err
		}
		count = n
		return nil
	})
	return count, err
}

type leaseState struct {
	status   Status
	version  int
	attempts int
	leasedBy *string
	expires  time.Time
}

func (l leaseState) checkOwner(workerID string) error {
	if l.status != StatusInAgent || l.leasedBy == nil {
		return ErrNotLeased
	}
	if *l.leasedBy != workerID {
		return fmt.Errorf("%w: held by %s", ErrNotLeaseOwner, *l.leasedBy)
	}
	return nil
}

func (s *Service) lockLease(ctx context.Context, tx pgx.Tx, caseID string) (leaseState, error) {
	var (
		state   leaseState
		status  string
		expires *time.Time
	)
	err := tx.QueryRow(ctx,
		`SELECT status, version, attempts, leased_by, lease_expires_at
		   FROM cases WHERE id = $1::uuid FOR UPDATE`, caseID).
		Scan(&status, &state.version, &state.attempts, &state.leasedBy, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return leaseState{}, ErrCaseNotFound
	}
	if err != nil {
		return leaseState{}, fmt.Errorf("cases: lock lease: %w", err)
	}
	state.status = Status(status)
	if expires != nil {
		state.expires = *expires
	}
	return state, nil
}

// transitionStatus routes one case status change through the
// internal/cases/state authority. The caller owns the transaction and has
// already locked the row; the authority re-locks it, validates the edge, actor,
// reason, and resolution, bumps version once, and writes the single
// case.transition audit. This package keeps its existing lease/attempt/reclaim
// audits around the call.
func (s *Service) transitionStatus(
	ctx context.Context, tx pgx.Tx, caseID string, to Status,
	expectedVersion int, reason string, now time.Time,
) (casestate.Snapshot, error) {
	snapshot, err := casestate.Transition(ctx, tx, casestate.TransitionRequest{
		CaseID:          caseID,
		To:              casestate.Status(to),
		ExpectedVersion: expectedVersion,
		Actor:           s.actor.String(),
		Reason:          reason,
		Now:             now,
	})
	if err != nil {
		return casestate.Snapshot{}, caseStateError(err)
	}
	return snapshot, nil
}

// incrementAttempts adds one attempt under the row lock. The status transition
// already incremented version, so this update must not bump it again.
func incrementAttempts(ctx context.Context, tx pgx.Tx, caseID string, version int) (int, error) {
	var attempts int
	err := tx.QueryRow(ctx,
		`UPDATE cases
		    SET attempts = attempts + 1
		  WHERE id = $1::uuid AND version = $2
		  RETURNING attempts`, caseID, version).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: attempts for %s", ErrStale, caseID)
	}
	if err != nil {
		return 0, fmt.Errorf("cases: increment attempts for %s: %w", caseID, err)
	}
	return attempts, nil
}

type expiredLease struct {
	id       string
	status   Status
	version  int
	attempts int
	leasedBy string
	expires  time.Time
}

func (s *Service) reclaimExpiredTx(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, status, version, attempts, leased_by, lease_expires_at
		   FROM cases
		  WHERE leased_by IS NOT NULL AND lease_expires_at <= $1
		  ORDER BY lease_expires_at, id
		  FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return 0, fmt.Errorf("cases: scan expired leases: %w", err)
	}

	expired := make([]expiredLease, 0)
	for rows.Next() {
		var (
			e       expiredLease
			status  string
			expires time.Time
		)
		if err := rows.Scan(&e.id, &status, &e.version, &e.attempts, &e.leasedBy, &expires); err != nil {
			rows.Close()
			return 0, fmt.Errorf("cases: scan expired lease: %w", err)
		}
		e.status = Status(status)
		e.expires = expires
		expired = append(expired, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("cases: scan expired leases: %w", err)
	}
	rows.Close()

	for _, e := range expired {
		attempts := e.attempts + 1
		status := StatusOpen
		reason := ""
		if attempts >= s.maxAttempts {
			status = StatusEscalated
			reason = casestate.EscalationAttemptsExhausted
		}

		snapshot, err := s.transitionStatus(ctx, tx, e.id, status, e.version, reason, now)
		if err != nil {
			if errors.Is(err, ErrStale) {
				return 0, fmt.Errorf("%w: reclaim %s", ErrStale, e.id)
			}
			return 0, err
		}
		newAttempts, err := incrementAttempts(ctx, tx, e.id, snapshot.Version)
		if err != nil {
			return 0, err
		}
		after := map[string]any{
			"status":   string(status),
			"version":  snapshot.Version,
			"attempts": newAttempts,
		}
		if reason != "" {
			after["escalation_reason"] = reason
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      s.actor,
			Action:     ActionCaseReclaim,
			EntityType: EntityCase,
			EntityID:   e.id,
			Before: map[string]any{
				"status":           string(e.status),
				"version":          e.version,
				"attempts":         e.attempts,
				"leased_by":        e.leasedBy,
				"lease_expires_at": e.expires,
			},
			After:     after,
			Timestamp: now,
		}); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}
