package state_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/cases/state"
)

// TestAuditConstantsMatchCasesPackage guards the one value that must stay
// byte-identical while the state package cannot import internal/cases: the
// case.transition action and the case entity type already used by the legacy
// permissive transition. Wave 1b deletes the duplicate legacy write, but the
// audit vocabulary in existing rows must not change.
func TestAuditConstantsMatchCasesPackage(t *testing.T) {
	if state.ActionCaseTransition != cases.ActionCaseTransition {
		t.Fatalf("state.ActionCaseTransition = %q, want %q",
			state.ActionCaseTransition, cases.ActionCaseTransition)
	}
	if state.ActionCaseTransition != "case.transition" {
		t.Fatalf("ActionCaseTransition = %q, want case.transition", state.ActionCaseTransition)
	}
	if state.EntityCase != cases.EntityCase {
		t.Fatalf("state.EntityCase = %q, want %q", state.EntityCase, cases.EntityCase)
	}
}
