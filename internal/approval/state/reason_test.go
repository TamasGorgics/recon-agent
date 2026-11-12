package state_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

func TestReasonRules(t *testing.T) {
	for _, edge := range state.Edges() {
		required := edge.To == state.StatusRejected ||
			edge.To == state.StatusApplyFailed ||
			edge.To == state.StatusSuperseded
		if edge.ReasonRequired != required {
			t.Errorf("%s -> %s ReasonRequired = %t, want %t",
				edge.From, edge.To, edge.ReasonRequired, required)
		}
		if edge.ReasonRequired {
			if err := edge.ValidateReason(""); !errors.Is(err, state.ErrReasonRequired) {
				t.Errorf("%s -> %s empty reason error = %v, want ErrReasonRequired",
					edge.From, edge.To, err)
			}
			if err := edge.ValidateReason("   \t "); !errors.Is(err, state.ErrReasonRequired) {
				t.Errorf("%s -> %s whitespace reason error = %v, want ErrReasonRequired",
					edge.From, edge.To, err)
			}
		} else if err := edge.ValidateReason(""); err != nil {
			t.Errorf("%s -> %s optional empty reason error = %v, want nil",
				edge.From, edge.To, err)
		}
		if len(edge.ReasonEnum) == 0 {
			if err := edge.ValidateReason("  valid reason  "); err != nil {
				t.Errorf("%s -> %s trimmed reason error = %v", edge.From, edge.To, err)
			}
		}
	}

	for _, edge := range state.Edges() {
		if edge.To != state.StatusApplyFailed {
			if len(edge.ReasonEnum) != 0 {
				t.Errorf("%s -> %s has a reason enum, want none", edge.From, edge.To)
			}
			continue
		}
		if len(edge.ReasonEnum) != 3 {
			t.Fatalf("%s -> %s enum = %v, want three values", edge.From, edge.To, edge.ReasonEnum)
		}
		for _, allowed := range []string{"state_changed", "break_resolved", "expired"} {
			if err := edge.ValidateReason(allowed); err != nil {
				t.Errorf("%s -> %s reason %q error = %v", edge.From, edge.To, allowed, err)
			}
			if err := edge.ValidateReason("  " + allowed + "  "); err != nil {
				t.Errorf("%s -> %s trimmed reason %q error = %v", edge.From, edge.To, allowed, err)
			}
		}
		for _, rejected := range []string{"other", "STATE_CHANGED", "state changed"} {
			if err := edge.ValidateReason(rejected); !errors.Is(err, state.ErrInvalidReason) {
				t.Errorf("%s -> %s reason %q error = %v, want ErrInvalidReason",
					edge.From, edge.To, rejected, err)
			}
		}
	}

	edge, ok := state.Lookup(state.StatusProposed, state.StatusPendingApproval)
	if !ok {
		t.Fatal("proposed -> pending_approval missing")
	}
	if err := edge.ValidateReason(strings.Repeat("a", 2000)); err != nil {
		t.Errorf("2000-rune reason error = %v, want nil", err)
	}
	if err := edge.ValidateReason(strings.Repeat("a", 2001)); !errors.Is(err, state.ErrInvalidReason) {
		t.Errorf("2001-rune reason error = %v, want ErrInvalidReason", err)
	}
	if err := edge.ValidateReason(strings.Repeat("é", 2000)); err != nil {
		t.Errorf("2000-rune multibyte reason error = %v, want nil", err)
	}
	for _, bad := range []string{"line\nbreak", "tab\tinside", "nul\x00byte", "carriage\rreturn"} {
		if err := edge.ValidateReason(bad); !errors.Is(err, state.ErrInvalidReason) {
			t.Errorf("reason %q error = %v, want ErrInvalidReason", bad, err)
		}
	}
}
