//go:build !faultinject

// covers: FIN-25

package fault_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/fault"
)

// TestFaultHitStubIsNoop proves the production build ignores a configured exit
// failpoint.
func TestFaultHitStubIsNoop(t *testing.T) {
	t.Setenv("RECON_FAULT", fault.ExecutorAfterProposalLock+":exit")
	fault.Hit(fault.ExecutorAfterProposalLock)
}
