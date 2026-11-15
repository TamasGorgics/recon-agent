//go:build faultinject

// covers: FIN-25

package fault_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/fault"
)

// TestFaultHitNonMatchingPointIsNoop sets a real action for one point and hits
// a different point.
func TestFaultHitNonMatchingPointIsNoop(t *testing.T) {
	t.Setenv("RECON_FAULT", fault.ExecutorAfterTransition+":panic")
	fault.Hit(fault.ExecutorBeforeCommit)
}

// TestFaultHitMalformedIsNoop covers a missing action, a non-matching point,
// an unknown action, and a trailing colon.
func TestFaultHitMalformedIsNoop(t *testing.T) {
	for _, raw := range []string{
		fault.ExecutorAfterProposalLock,
		"other.point:exit",
		fault.ExecutorAfterProposalLock + ":bogus",
		fault.ExecutorAfterProposalLock + ":exit:extra",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("RECON_FAULT", raw)
			fault.Hit(fault.ExecutorAfterProposalLock)
		})
	}
}

// TestFaultHitMatchingPanic panics with the failpoint name.
func TestFaultHitMatchingPanic(t *testing.T) {
	const point = fault.ExecutorAfterLedgerInsert
	t.Setenv("RECON_FAULT", point+":panic")

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("Hit(%q) returned, want panic", point)
		}
		if got, ok := recovered.(string); !ok || got != point {
			t.Fatalf("recovered %#v, want panic value %q", recovered, point)
		}
	}()
	fault.Hit(point)
}

// TestFaultHitMatchingExitExits86 re-executes this test binary so the os.Exit
// call cannot take down the parent test process.
func TestFaultHitMatchingExitExits86(t *testing.T) {
	const point = fault.ExecutorBeforeCommit
	if os.Getenv("RECON_FAULT_TEST_SUBPROCESS") == "1" {
		fault.Hit(point)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFaultHitMatchingExitExits86$")
	cmd.Env = faultTestEnv(
		"RECON_FAULT_TEST_SUBPROCESS=1",
		"RECON_FAULT="+point+":exit",
	)
	err := cmd.Run()
	if err == nil {
		t.Fatal("subprocess exited 0, want exit status 86")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("subprocess error = %v, want *exec.ExitError", err)
	}
	if code := exitErr.ExitCode(); code != 86 {
		t.Fatalf("subprocess exit code = %d, want 86", code)
	}
}

// faultTestEnv returns the current environment without the failpoint
// variables, then appends the child-specific values.
func faultTestEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "RECON_FAULT" || key == "RECON_FAULT_TEST_SUBPROCESS" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, extra...)
}
