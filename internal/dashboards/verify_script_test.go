// covers: FIN-M5-05-33
package dashboards_test

import (
	"strings"
	"testing"
)

// TestVerifyDashboardScriptCoversBothDashboards is the static half of
// FIN-M5-05-33: the live verifier must iterate the M5-04 and M5-05 UIDs, deny
// anonymous dashboard reads, and fail an empty seeded panel instead of only
// checking that a query executed.
func TestVerifyDashboardScriptCoversBothDashboards(t *testing.T) {
	script := string(readRepoFile(t, "scripts/verify-dashboard.sh"))
	for _, want := range []string{
		"recon-ops",
		"Recon Ops",
		"recon-agent-quality",
		"Recon Agent Quality",
		"HTTP 401",
		"anonymous dashboard API",
		"target_rows == 0",
		"dashboard {uid} panel",
		"escalation-reasons table",
		"42501",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("verify-dashboard.sh does not contain %q", want)
		}
	}
	if strings.Contains(script, "Panel data may be empty") {
		t.Error("verify-dashboard.sh still documents empty seeded panels")
	}
}
