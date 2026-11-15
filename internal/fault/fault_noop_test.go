// covers: FIN-25

package fault_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/fault"
)

// TestFaultHitWithoutEnvIsNoop runs in every build: with no RECON_FAULT the
// call must return instead of exiting or panicking.
func TestFaultHitWithoutEnvIsNoop(t *testing.T) {
	t.Setenv("RECON_FAULT", "")
	fault.Hit(fault.ExecutorAfterProposalLock)
}
