package agent_test

import (
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// TestDeterministicRunID pins the replay run-id contract: same case id, same
// valid UUID, different case ids differ.
func TestDeterministicRunID(t *testing.T) {
	const (
		caseA = "10000000-0000-4000-8000-000000000001"
		caseB = "10000000-0000-4000-8000-000000000002"
	)
	first, err := agent.DeterministicRunID(caseA)
	if err != nil {
		t.Fatalf("DeterministicRunID: %v", err)
	}
	second, err := agent.DeterministicRunID(caseA)
	if err != nil {
		t.Fatalf("DeterministicRunID: %v", err)
	}
	if first != second {
		t.Fatalf("run id is not deterministic: %q != %q", first, second)
	}
	if !tools.IsUUID(first) {
		t.Fatalf("run id %q is not a UUID", first)
	}
	other, err := agent.DeterministicRunID(caseB)
	if err != nil {
		t.Fatalf("DeterministicRunID: %v", err)
	}
	if other == first {
		t.Fatalf("different case ids produced the same run id %q", first)
	}
	if _, err := agent.DeterministicRunID("not-a-uuid"); err == nil {
		t.Fatal("DeterministicRunID accepted a non-UUID case id")
	}
}
