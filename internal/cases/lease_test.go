//go:build integration

// covers: FIN-24, FIN-30

package cases_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/cases"
)

// seedOneCase creates one external-only break in its own settlement batch and
// returns the resulting case, so each case is a singleton group.
func seedOneCase(t *testing.T, f *fixture, recordID string, amountMinor int64) cases.Case {
	t.Helper()
	batch := insertBatch(t, f.db)
	ref := f.addExternal(t, recordID, "payout", -amountMinor, batch)
	result := f.createBreaks(t, externalOnlyCandidate(t, f.scopeID, "USD", ref, amountMinor))
	if len(result.Cases) != 1 {
		t.Fatalf("seed %s: cases = %d, want 1", recordID, len(result.Cases))
	}
	return result.Cases[0]
}

// TestLeaseOrderAndRenewRelease exercises the happy path: the queue is ordered
// by priority, a lease takes the case out of the pool, the owner renews and
// releases it, and every transition is audited.
func TestLeaseOrderAndRenewRelease(t *testing.T) {
	f := newFixture(t)
	low := seedOneCase(t, f, "lease-low", 100)
	high := seedOneCase(t, f, "lease-high", 5_000_000)

	leased, err := f.service.Lease(context.Background(), "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if leased.ID != high.ID {
		t.Fatalf("leased case = %s, want highest-priority %s", leased.ID, high.ID)
	}
	if leased.Status != cases.StatusInAgent {
		t.Fatalf("leased status = %s, want %s", leased.Status, cases.StatusInAgent)
	}
	if leased.LeasedBy == nil || *leased.LeasedBy != "worker-a" {
		t.Fatalf("leased_by = %v, want worker-a", leased.LeasedBy)
	}
	wantExpiry := f.clock.Now().Add(time.Minute)
	if leased.LeaseExpiresAt == nil || !leased.LeaseExpiresAt.Equal(wantExpiry) {
		t.Fatalf("lease_expires_at = %v, want %v", leased.LeaseExpiresAt, wantExpiry)
	}
	if got, want := leased.Version, 1; got != want {
		t.Fatalf("version after lease = %d, want %d", got, want)
	}
	if got, want := leased.Attempts, 0; got != want {
		t.Fatalf("attempts after lease = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, leased.ID, cases.ActionCaseLease), 1; got != want {
		t.Fatalf("lease audit rows = %d, want %d", got, want)
	}

	// The lower-priority case is next; the leased case is not handed out again.
	next, err := f.service.Lease(context.Background(), "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("Lease next: %v", err)
	}
	if next.ID != low.ID {
		t.Fatalf("second lease = %s, want %s", next.ID, low.ID)
	}
	if _, err := f.service.Lease(context.Background(), "worker-c", time.Minute); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("empty queue Lease error = %v, want ErrNoCase", err)
	}

	renewed, err := f.service.Renew(context.Background(), leased.ID, "worker-a", 2*time.Minute)
	if err != nil {
		t.Fatalf("Renew owner: %v", err)
	}
	wantRenewed := f.clock.Now().Add(2 * time.Minute)
	if renewed.LeaseExpiresAt == nil || !renewed.LeaseExpiresAt.Equal(wantRenewed) {
		t.Fatalf("renewed expiry = %v, want %v", renewed.LeaseExpiresAt, wantRenewed)
	}
	if got, want := renewed.Version, 2; got != want {
		t.Fatalf("version after renew = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, leased.ID, cases.ActionCaseRenew), 1; got != want {
		t.Fatalf("renew audit rows = %d, want %d", got, want)
	}

	if _, err := f.service.Renew(context.Background(), leased.ID, "worker-b", time.Minute); !errors.Is(err, cases.ErrNotLeaseOwner) {
		t.Fatalf("Renew non-owner error = %v, want ErrNotLeaseOwner", err)
	}
	if _, err := f.service.Release(context.Background(), leased.ID, "worker-b"); !errors.Is(err, cases.ErrNotLeaseOwner) {
		t.Fatalf("Release non-owner error = %v, want ErrNotLeaseOwner", err)
	}
	unchanged := f.getCase(t, leased.ID)
	if got, want := unchanged.Version, 2; got != want {
		t.Fatalf("version after rejected renew/release = %d, want %d", got, want)
	}

	released, err := f.service.Release(context.Background(), leased.ID, "worker-a")
	if err != nil {
		t.Fatalf("Release owner: %v", err)
	}
	if released.Status != cases.StatusOpen || released.LeasedBy != nil || released.LeaseExpiresAt != nil {
		t.Fatalf("released case = %+v, want open with no lease", released)
	}
	if got, want := released.Version, 3; got != want {
		t.Fatalf("version after release = %d, want %d", got, want)
	}
	if got, want := released.Attempts, 0; got != want {
		t.Fatalf("attempts after release = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, leased.ID, cases.ActionCaseRelease), 1; got != want {
		t.Fatalf("release audit rows = %d, want %d", got, want)
	}
	if _, err := f.service.Release(context.Background(), leased.ID, "worker-a"); !errors.Is(err, cases.ErrNotLeased) {
		t.Fatalf("double release error = %v, want ErrNotLeased", err)
	}
	if got, want := f.auditCount(t, leased.ID, cases.ActionCaseRelease), 1; got != want {
		t.Fatalf("release audit rows after double release = %d, want %d", got, want)
	}
}

// TestLeaseRejectsNonPositiveTTL covers the TTL <= 0 edge case.
func TestLeaseRejectsNonPositiveTTL(t *testing.T) {
	f := newFixture(t)
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := f.service.Lease(context.Background(), "worker-a", ttl); !errors.Is(err, cases.ErrInvalidTTL) {
			t.Fatalf("Lease(ttl=%v) error = %v, want ErrInvalidTTL", ttl, err)
		}
	}
}

// TestExpiredLeaseIsReclaimed covers expiry: the case returns to the open pool,
// attempts increments once, the version increments, and the reclaim is audited.
func TestExpiredLeaseIsReclaimed(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "lease-expiry", 900)
	leased, err := f.service.Lease(context.Background(), "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	f.clock.Advance(2 * time.Minute)

	reclaimed, err := f.service.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if got, want := reclaimed, 1; got != want {
		t.Fatalf("reclaimed = %d, want %d", got, want)
	}
	after := f.getCase(t, leased.ID)
	if after.Status != cases.StatusOpen || after.LeasedBy != nil || after.LeaseExpiresAt != nil {
		t.Fatalf("reclaimed case = %+v, want open with no lease", after)
	}
	if got, want := after.Attempts, 1; got != want {
		t.Fatalf("attempts after reclaim = %d, want %d", got, want)
	}
	if got, want := after.Version, 2; got != want {
		t.Fatalf("version after reclaim = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, leased.ID, cases.ActionCaseReclaim), 1; got != want {
		t.Fatalf("reclaim audit rows = %d, want %d", got, want)
	}

	// The reclaimed case is leaseable again and the second lease does not
	// increment attempts; only expiry does (spec section 6.5).
	released, err := f.service.Lease(context.Background(), "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("Lease after reclaim: %v", err)
	}
	if released.ID != seeded.ID {
		t.Fatalf("released case = %s, want %s", released.ID, seeded.ID)
	}
	if got, want := released.Attempts, 1; got != want {
		t.Fatalf("attempts after second lease = %d, want %d", got, want)
	}
	if got, want := released.Version, 3; got != want {
		t.Fatalf("version after second lease = %d, want %d", got, want)
	}
}

// TestReclaimExpiredEscalatesAtAttemptCap proves an expired lease counts as an
// attempt and that reaching the cap escalates agent_failed instead of leaving
// the case open for another lease.
func TestReclaimExpiredEscalatesAtAttemptCap(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "lease-reclaim-cap", 900)

	for attempt := 1; attempt <= 2; attempt++ {
		leased, err := f.service.Lease(context.Background(), "worker-a", time.Minute)
		if err != nil {
			t.Fatalf("Lease attempt %d: %v", attempt, err)
		}
		if leased.ID != seeded.ID {
			t.Fatalf("leased case attempt %d = %s, want %s", attempt, leased.ID, seeded.ID)
		}
		f.clock.Advance(2 * time.Minute)
		reclaimed, err := f.service.ReclaimExpired(context.Background())
		if err != nil {
			t.Fatalf("ReclaimExpired attempt %d: %v", attempt, err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimed attempt %d = %d, want 1", attempt, reclaimed)
		}
		after := f.getCase(t, seeded.ID)
		if after.Status != cases.StatusOpen || after.Attempts != attempt {
			t.Fatalf("case after reclaim %d = %s/%d, want open/%d",
				attempt, after.Status, after.Attempts, attempt)
		}
	}

	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("Lease final attempt: %v", err)
	}
	f.clock.Advance(2 * time.Minute)
	reclaimed, err := f.service.ReclaimExpired(context.Background())
	if err != nil {
		t.Fatalf("ReclaimExpired final: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed final = %d, want 1", reclaimed)
	}

	final := f.getCase(t, seeded.ID)
	if final.Status != cases.StatusEscalated {
		t.Fatalf("final status = %s, want escalated", final.Status)
	}
	if final.Attempts != 3 {
		t.Fatalf("final attempts = %d, want 3", final.Attempts)
	}
	if final.LeasedBy != nil || final.LeaseExpiresAt != nil {
		t.Fatalf("escalated case still leased: %v/%v", final.LeasedBy, final.LeaseExpiresAt)
	}
	if final.EscalationReason == nil || *final.EscalationReason != "agent_failed" {
		t.Fatalf("escalation reason = %v, want agent_failed", final.EscalationReason)
	}
	if got, want := f.auditCount(t, seeded.ID, cases.ActionCaseReclaim), 3; got != want {
		t.Fatalf("reclaim audit rows = %d, want %d", got, want)
	}

	var afterStatus, afterReason *string
	if err := f.db.Pool.QueryRow(context.Background(),
		`SELECT after->>'status', after->>'escalation_reason'
		   FROM audit_log WHERE entity_id = $1 AND action = $2
		  ORDER BY seq DESC LIMIT 1`, seeded.ID, cases.ActionCaseReclaim).
		Scan(&afterStatus, &afterReason); err != nil {
		t.Fatalf("read final reclaim audit: %v", err)
	}
	if afterStatus == nil || *afterStatus != string(cases.StatusEscalated) {
		t.Fatalf("final reclaim audit status = %v, want escalated", afterStatus)
	}
	if afterReason == nil || *afterReason != "agent_failed" {
		t.Fatalf("final reclaim audit reason = %v, want agent_failed", afterReason)
	}

	if _, err := f.service.Lease(context.Background(), "worker-b", time.Minute); !errors.Is(err, cases.ErrNoCase) {
		t.Fatalf("Lease after escalation = %v, want ErrNoCase", err)
	}
	if state := f.getCase(t, seeded.ID); state.Status != cases.StatusEscalated || state.Attempts != 3 {
		t.Fatalf("lease attempt after escalation changed case: %s/%d", state.Status, state.Attempts)
	}
}

// TestLeaseReclaimsExpiredLeaseInline proves Lease does not stall behind an
// expired lease: the same call reclaims it, audits the reclaim, and hands it
// back to the new worker.
func TestLeaseReclaimsExpiredLeaseInline(t *testing.T) {
	f := newFixture(t)
	seeded := seedOneCase(t, f, "lease-inline", 400)
	if _, err := f.service.Lease(context.Background(), "worker-a", time.Minute); err != nil {
		t.Fatalf("first Lease: %v", err)
	}
	f.clock.Advance(2 * time.Minute)

	released, err := f.service.Lease(context.Background(), "worker-b", time.Minute)
	if err != nil {
		t.Fatalf("Lease after expiry: %v", err)
	}
	if released.ID != seeded.ID {
		t.Fatalf("released case = %s, want %s", released.ID, seeded.ID)
	}
	if released.LeasedBy == nil || *released.LeasedBy != "worker-b" {
		t.Fatalf("leased_by = %v, want worker-b", released.LeasedBy)
	}
	if got, want := released.Attempts, 1; got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, seeded.ID, cases.ActionCaseReclaim), 1; got != want {
		t.Fatalf("inline reclaim audit rows = %d, want %d", got, want)
	}
	if got, want := f.auditCount(t, seeded.ID, cases.ActionCaseLease), 2; got != want {
		t.Fatalf("lease audit rows = %d, want %d", got, want)
	}
}

// TestRenewExpiredLeaseFails ensures an expired lease cannot be extended by its
// former owner without going back through the queue.
func TestRenewExpiredLeaseFails(t *testing.T) {
	f := newFixture(t)
	seedOneCase(t, f, "lease-renew-expired", 100)
	leased, err := f.service.Lease(context.Background(), "worker-a", time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	f.clock.Advance(2 * time.Minute)
	if _, err := f.service.Renew(context.Background(), leased.ID, "worker-a", time.Minute); !errors.Is(err, cases.ErrLeaseExpired) {
		t.Fatalf("Renew expired error = %v, want ErrLeaseExpired", err)
	}
	if got, want := f.getCase(t, leased.ID).Version, 1; got != want {
		t.Fatalf("version after rejected expired renew = %d, want %d", got, want)
	}
}
