package state_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

// covers: FIN-24
func TestStatusSetMatchesSpec(t *testing.T) {
	want := []state.Status{
		state.StatusProposed,
		state.StatusPendingApproval,
		state.StatusApproved,
		state.StatusAutoApproved,
		state.StatusApplied,
		state.StatusApplyFailed,
		state.StatusRejected,
		state.StatusExpired,
		state.StatusSuperseded,
	}
	got := state.AllStatuses()
	if len(got) != len(want) {
		t.Fatalf("AllStatuses length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllStatuses[%d] = %q, want %q", i, got[i], want[i])
		}
		if !got[i].Valid() {
			t.Errorf("%q is not Valid", got[i])
		}
	}

	for _, invalid := range []state.Status{"", "escalated", "PROPOSED", "pending"} {
		if invalid.Valid() {
			t.Errorf("%q is Valid, want invalid", invalid)
		}
	}

	terminal := map[state.Status]bool{
		state.StatusApplied:    true,
		state.StatusRejected:   true,
		state.StatusExpired:    true,
		state.StatusSuperseded: true,
	}
	for _, s := range want {
		if state.Terminal(s) != terminal[s] {
			t.Errorf("Terminal(%q) = %t, want %t", s, state.Terminal(s), terminal[s])
		}
	}
	if state.Terminal(state.StatusApplyFailed) {
		t.Error("apply_failed must be non-terminal")
	}

	// AllStatuses returns a copy; mutating it cannot change the package set.
	got[0] = "mutated"
	if state.AllStatuses()[0] != state.StatusProposed {
		t.Fatal("AllStatuses returned a slice backed by the package status set")
	}
}
