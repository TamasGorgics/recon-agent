// covers: FIN-54

package tieout_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestNoMatcherDependency enforces the FIN-54 independence requirement: the
// tie-out package computes from its own SQL and must never import
// internal/match. `go list -deps .` is evaluated without -test so the matcher
// can still be used by the integration fixtures in this test package.
func TestNoMatcherDependency(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, output)
	}
	const forbidden = "github.com/TamasGorgics/recon-agent/internal/match"
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == forbidden {
			t.Fatalf("internal/tieout depends on internal/match:\n%s", output)
		}
	}
}
