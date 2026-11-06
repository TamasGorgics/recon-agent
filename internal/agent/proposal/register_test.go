package proposal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestProposalRegistration(t *testing.T) {
	t.Parallel()
	registry := tools.NewRegistry()
	execer := &fakeExecer{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if err := Register(registry, newUnitDeps(execer, &fakeEvidenceReader{})); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	want := []string{
		"escalate", "mark_duplicate", "propose_adjustment", "propose_link",
		"propose_write_off", "request_info",
	}
	got := registry.Names()
	if len(got) != len(want) {
		t.Fatalf("registered tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered tools = %v, want %v", got, want)
		}
	}

	adjustment, _ := registry.Lookup("propose_adjustment")
	if !strings.Contains(adjustment.Description(), "Does NOT post to the ledger.") {
		t.Fatalf("propose_adjustment description = %q", adjustment.Description())
	}
	writeOff, _ := registry.Lookup("propose_write_off")
	if !strings.Contains(writeOff.Description(), "Does NOT post to the ledger.") {
		t.Fatalf("propose_write_off description = %q", writeOff.Description())
	}
	escalate, _ := registry.Lookup("escalate")
	if !strings.Contains(escalate.Description(), "no proposal is recorded") {
		t.Fatalf("escalate description = %q", escalate.Description())
	}

	if err := Register(nil, newUnitDeps(execer, &fakeEvidenceReader{})); err == nil {
		t.Fatal("Register(nil) error = nil")
	}
	if err := Register(registry, newUnitDeps(execer, &fakeEvidenceReader{})); !errors.Is(err, tools.ErrDuplicateTool) {
		t.Fatalf("duplicate Register() error = %v, want ErrDuplicateTool", err)
	}
	if err := Register(tools.NewRegistry(), Deps{Evidence: &fakeEvidenceReader{}}); err == nil {
		t.Fatal("Register() without AgentExec error = nil")
	}
	if err := Register(tools.NewRegistry(), Deps{AgentExec: execer}); err == nil {
		t.Fatal("Register() without evidence source error = nil")
	}

	for _, name := range want {
		tool, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
			t.Fatalf("%s schema is invalid JSON: %v", name, err)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s schema is not closed", name)
		}
	}
}
