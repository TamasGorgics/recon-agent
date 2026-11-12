package state_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

func TestStateMachineDocIsCurrent(t *testing.T) {
	const regen = "go generate ./internal/approval/state"
	want, err := os.ReadFile("../../../docs/state-machine.md")
	if err != nil {
		t.Fatalf("read docs/state-machine.md: %v; run %s", err, regen)
	}
	got := state.RenderMarkdown()
	if !bytes.Equal(got, want) {
		t.Fatalf("docs/state-machine.md is stale; run %s", regen)
	}
}
