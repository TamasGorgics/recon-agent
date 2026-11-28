// covers: FIN-52
package integrity_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoMatcherDependency enforces the FIN-52 independence requirement: the
// integrity package computes from raw tables and the independent
// invariants/audit/tieout APIs, never from internal/match.
func TestNoMatcherDependency(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, output)
	}
	const forbidden = "github.com/TamasGorgics/recon-agent/internal/match"
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == forbidden {
			t.Fatalf("internal/integrity depends on internal/match:\n%s", output)
		}
	}
}

// TestNoDirectWallClock enforces the domain clock convention: production files
// in this package must use the injected clock, never time.Now or time.Sleep.
func TestNoDirectWallClock(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(source)
		for _, forbidden := range []string{"time.Now(", "time.Sleep("} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains %s; use the injected clock", file, forbidden)
			}
		}
	}
}
