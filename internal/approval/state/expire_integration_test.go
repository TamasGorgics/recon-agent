//go:build integration

// covers: FIN-43, FIN-30
package state_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

// TestExpireDueBoundaries covers FIN-43: the injected clock drives an inclusive
// expires_at <= now boundary, and only proposed/pending_approval rows with a
// non-NULL expiry are eligible.
func TestExpireDueBoundaries(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	// Postgres timestamptz stores microsecond precision, so one microsecond is
	// the finest representable step around the inclusive boundary. At clock=now,
	// a row expiring at now+1us is one step before the boundary and is untouched.
	notYet := f.seed(t, state.StatusProposed, withExpiry(now.Add(time.Microsecond)))
	atProposed := f.seed(t, state.StatusProposed, withExpiry(now))
	atPending := f.seed(t, state.StatusPendingApproval, withExpiry(now))
	later := f.seed(t, state.StatusProposed, withExpiry(now.Add(time.Second)))
	nullExpiry := f.seed(t, state.StatusProposed)
	approved := f.seed(t, state.StatusApproved, withExpiry(now))
	autoApproved := f.seed(t, state.StatusAutoApproved, withExpiry(now))
	applyFailed := f.seed(t, state.StatusApplyFailed, withExpiry(now))
	rejected := f.seed(t, state.StatusRejected, withExpiry(now))
	superseded := f.seed(t, state.StatusSuperseded, withExpiry(now))

	count, err := f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if count != 2 {
		t.Fatalf("ExpireDue count = %d, want 2 (equality boundary only)", count)
	}
	for _, id := range []string{atProposed, atPending} {
		stored := f.get(t, id)
		if stored.Status != state.StatusExpired || stored.Version != 1 {
			t.Fatalf("due proposal = %s/version %d, want expired/1", stored.Status, stored.Version)
		}
		rows := f.audits(t, id)
		if len(rows) != 1 {
			t.Fatalf("due proposal audit rows = %d, want 1", len(rows))
		}
		if rows[0].ActorType != "system" || rows[0].ActorID != "expiry" {
			t.Fatalf("expiry actor = %s:%s, want system:expiry", rows[0].ActorType, rows[0].ActorID)
		}
		if jsonObject(t, rows[0].After)["reason"] != "expired" {
			t.Fatalf("expiry after.reason = %v, want expired", jsonObject(t, rows[0].After)["reason"])
		}
		if !rows[0].Timestamp.Equal(now.Truncate(time.Microsecond)) {
			t.Fatalf("expiry timestamp = %s, want %s", rows[0].Timestamp, now.Truncate(time.Microsecond))
		}
	}
	for name, id := range map[string]string{
		"not_yet_due":   notYet,
		"later":         later,
		"null_expiry":   nullExpiry,
		"approved":      approved,
		"auto_approved": autoApproved,
		"apply_failed":  applyFailed,
		"rejected":      rejected,
		"superseded":    superseded,
	} {
		stored := f.get(t, id)
		if stored.Version != 0 {
			t.Errorf("%s version = %d, want 0 (not expired)", name, stored.Version)
		}
		if stored.Status == state.StatusExpired {
			t.Errorf("%s was expired, want untouched", name)
		}
		if got := f.auditCount(t, id); got != 0 {
			t.Errorf("%s audit rows = %d, want 0", name, got)
		}
	}

	// Advance exactly onto the next row's expires_at: the boundary is inclusive.
	f.clock.Set(now.Add(time.Microsecond))
	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("ExpireDue after +1us: %v", err)
	}
	if count != 1 {
		t.Fatalf("ExpireDue after +1us count = %d, want 1", count)
	}
	if stored := f.get(t, notYet); stored.Status != state.StatusExpired || stored.Version != 1 {
		t.Fatalf("not-yet proposal = %s/version %d, want expired/1", stored.Status, stored.Version)
	}
	if stored := f.get(t, later); stored.Version != 0 || stored.Status == state.StatusExpired {
		t.Fatalf("later proposal = %s/version %d, want untouched", stored.Status, stored.Version)
	}

	f.clock.Set(now.Add(time.Second))
	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("ExpireDue at +1s: %v", err)
	}
	if count != 1 {
		t.Fatalf("ExpireDue at +1s count = %d, want 1", count)
	}
	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("fourth ExpireDue: %v", err)
	}
	if count != 0 {
		t.Fatalf("fourth ExpireDue count = %d, want 0", count)
	}
}

// TestExpireDueIdempotent proves a second call is a no-op and each committed
// row has one system:expiry audit entry and exactly one version bump.
func TestExpireDueIdempotent(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	ids := []string{
		f.seed(t, state.StatusProposed, withExpiry(now.Add(-time.Hour))),
		f.seed(t, state.StatusPendingApproval, withExpiry(now.Add(-time.Minute))),
		f.seed(t, state.StatusProposed, withExpiry(now)),
	}

	count, err := f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("first ExpireDue: %v", err)
	}
	if count != 3 {
		t.Fatalf("first ExpireDue count = %d, want 3", count)
	}
	for _, id := range ids {
		stored := f.get(t, id)
		if stored.Status != state.StatusExpired || stored.Version != 1 {
			t.Fatalf("proposal = %s/version %d, want expired/1", stored.Status, stored.Version)
		}
		rows := f.audits(t, id)
		if len(rows) != 1 {
			t.Fatalf("audit rows = %d, want 1", len(rows))
		}
		if rows[0].Action != state.ActionProposalTransition {
			t.Fatalf("audit action = %q, want %q", rows[0].Action, state.ActionProposalTransition)
		}
		before := jsonObject(t, rows[0].Before)
		after := jsonObject(t, rows[0].After)
		if before["status"] != string(state.StatusProposed) &&
			before["status"] != string(state.StatusPendingApproval) {
			t.Fatalf("before.status = %v, want proposed or pending_approval", before["status"])
		}
		if before["version"] != float64(0) || after["version"] != float64(1) {
			t.Fatalf("audit versions = %v -> %v, want 0 -> 1", before["version"], after["version"])
		}
	}

	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("second ExpireDue: %v", err)
	}
	if count != 0 {
		t.Fatalf("second ExpireDue count = %d, want 0", count)
	}
	for _, id := range ids {
		if got := f.auditCount(t, id); got != 1 {
			t.Fatalf("second call changed audit rows = %d, want 1", got)
		}
	}
}

// TestExpireDueRacesApprove races expiry against a human approval. Exactly one
// transition wins and exactly one audit row survives.
func TestExpireDueRacesApprove(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	human := humanActor(t, "alice")
	for iteration := 0; iteration < 10; iteration++ {
		id := f.seed(t, state.StatusPendingApproval, withExpiry(now))

		var (
			wg           sync.WaitGroup
			approve      state.Proposal
			approveErr   error
			expiredCount int
			expireErr    error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			approve, approveErr = f.transition(state.TransitionRequest{
				ProposalID:      id,
				To:              state.StatusApproved,
				ExpectedVersion: 0,
				Actor:           human,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			expiredCount, expireErr = f.service.ExpireDue(context.Background())
		}()
		close(start)
		wg.Wait()

		if expireErr != nil {
			t.Fatalf("iteration %d: ExpireDue: %v", iteration, expireErr)
		}
		if approveErr != nil {
			switch {
			case errors.Is(approveErr, state.ErrStale),
				errors.Is(approveErr, state.ErrTerminalState),
				errors.Is(approveErr, state.ErrIllegalTransition):
			default:
				t.Fatalf("iteration %d: approve error = %v, want a typed loser error", iteration, approveErr)
			}
			if expiredCount != 1 {
				t.Fatalf("iteration %d: expiry count = %d, want 1 when approve lost", iteration, expiredCount)
			}
			if stored := f.get(t, id); stored.Status != state.StatusExpired {
				t.Fatalf("iteration %d: final status = %s, want expired", iteration, stored.Status)
			}
		} else {
			if expiredCount != 0 {
				t.Fatalf("iteration %d: expiry count = %d, want 0 when approve won", iteration, expiredCount)
			}
			if approve.Status != state.StatusApproved || approve.Version != 1 {
				t.Fatalf("iteration %d: approve result = %s/version %d, want approved/1",
					iteration, approve.Status, approve.Version)
			}
			if stored := f.get(t, id); stored.Status != state.StatusApproved {
				t.Fatalf("iteration %d: final status = %s, want approved", iteration, stored.Status)
			}
		}
		if stored := f.get(t, id); stored.Version != 1 {
			t.Fatalf("iteration %d: final version = %d, want 1", iteration, stored.Version)
		}
		if got := f.auditCount(t, id); got != 1 {
			t.Fatalf("iteration %d: audit rows = %d, want 1", iteration, got)
		}
	}
}

// TestExpireDueBatchBound proves one call commits at most 100 transitions and
// a second drains the rest without duplicates.
func TestExpireDueBatchBound(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	const total = 101
	for i := 0; i < total; i++ {
		f.seed(t, state.StatusProposed, withExpiry(now))
	}

	count, err := f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("first ExpireDue: %v", err)
	}
	if count != 100 {
		t.Fatalf("first ExpireDue count = %d, want 100", count)
	}
	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("second ExpireDue: %v", err)
	}
	if count != 1 {
		t.Fatalf("second ExpireDue count = %d, want 1", count)
	}
	count, err = f.service.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("third ExpireDue: %v", err)
	}
	if count != 0 {
		t.Fatalf("third ExpireDue count = %d, want 0", count)
	}

	var expired, versionOne, auditRows int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM proposals WHERE status = 'expired'`).Scan(&expired); err != nil {
		t.Fatalf("count expired: %v", err)
	}
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM proposals WHERE status = 'expired' AND version = 1`).Scan(&versionOne); err != nil {
		t.Fatalf("count version one: %v", err)
	}
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1`, state.ActionProposalTransition).
		Scan(&auditRows); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if expired != total || versionOne != total || auditRows != total {
		t.Fatalf("expired=%d versionOne=%d auditRows=%d, want %d each",
			expired, versionOne, auditRows, total)
	}
}
