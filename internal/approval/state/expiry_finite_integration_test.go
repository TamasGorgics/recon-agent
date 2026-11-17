//go:build integration

// covers: FIN-30
package state_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

// setRawExpiry writes a non-finite timestamp through the migration/admin pool.
// The app role cannot write expires_at for arbitrary values, and Go's
// time.Time cannot represent PostgreSQL infinity.
func (f *fixture) setRawExpiry(t *testing.T, proposalID, value string) {
	t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET expires_at = $2::timestamptz WHERE id = $1::uuid`,
		proposalID, value); err != nil {
		t.Fatalf("set expires_at %q: %v", value, err)
	}
}

// TestTransitionRejectsNonFiniteExpiry is the Q-85(1) regression guard: a row
// whose expires_at is +/-infinity must produce the typed ErrNonFiniteExpiry,
// not a raw pgx scan error, and must change nothing.
func TestTransitionRejectsNonFiniteExpiry(t *testing.T) {
	for _, value := range []string{"-infinity", "infinity"} {
		t.Run(value, func(t *testing.T) {
			f := newFixture(t)
			proposalID := f.seed(t, state.StatusProposed)
			f.setRawExpiry(t, proposalID, value)

			_, err := f.transition(state.TransitionRequest{
				ProposalID:      proposalID,
				To:              state.StatusPendingApproval,
				ExpectedVersion: 0,
				Actor:           systemActor(t, "approval"),
				Reason:          "approval_required",
			})
			if !errors.Is(err, state.ErrNonFiniteExpiry) {
				t.Fatalf("Transition error = %v, want ErrNonFiniteExpiry", err)
			}

			status, version := f.statusVersion(t, proposalID)
			if status != string(state.StatusProposed) || version != 0 {
				t.Fatalf("proposal after rejected Transition = %s/version %d, want proposed/0",
					status, version)
			}
			if count := f.auditCount(t, proposalID); count != 0 {
				t.Fatalf("audit rows after rejected Transition = %d, want 0", count)
			}
		})
	}
}

// statusVersion reads only finite-safe columns for a row whose expires_at may
// be +/-infinity.
func (f *fixture) statusVersion(t *testing.T, proposalID string) (string, int) {
	t.Helper()
	var (
		status  string
		version int
	)
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT status, version FROM proposals WHERE id = $1::uuid`, proposalID).
		Scan(&status, &version); err != nil {
		t.Fatalf("read status/version %s: %v", proposalID, err)
	}
	return status, version
}

// TestExpireDueSkipsNonFiniteExpiry proves a -infinity row cannot starve the
// expiry queue: it is not a candidate, and a healthy due row still drains in the
// same call.
func TestExpireDueSkipsNonFiniteExpiry(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	infinityID := f.seed(t, state.StatusProposed)
	f.setRawExpiry(t, infinityID, "-infinity")

	healthyID := f.seed(t, state.StatusProposed, withExpiry(f.clock.Now().Add(-time.Minute)))

	expired, err := f.service.ExpireDue(ctx)
	if err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if expired != 1 {
		t.Fatalf("ExpireDue expired %d proposals, want 1", expired)
	}

	if stored := f.get(t, healthyID); stored.Status != state.StatusExpired {
		t.Fatalf("healthy due proposal = %s, want expired", stored.Status)
	}
	if status, _ := f.statusVersion(t, infinityID); status != string(state.StatusProposed) {
		t.Fatalf("non-finite proposal = %s, want proposed (excluded from expiry)", status)
	}

	// The healthy row is drained; further calls are idempotent and ignore the
	// non-finite row.
	expired, err = f.service.ExpireDue(ctx)
	if err != nil {
		t.Fatalf("second ExpireDue: %v", err)
	}
	if expired != 0 {
		t.Fatalf("second ExpireDue expired %d proposals, want 0", expired)
	}
}
