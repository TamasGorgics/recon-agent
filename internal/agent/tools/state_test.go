package tools_test

import (
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func TestRunStateInitialized(t *testing.T) {
	t.Parallel()

	run := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	first := run.State()
	if first == nil {
		t.Fatal("NewRunContext().State() = nil, want a state")
	}
	if run.State() != first {
		t.Fatal("State() returned different pointers for the same context")
	}
	zero := &tools.RunContext{}
	if zero.State() == nil {
		t.Fatal("zero-value RunContext.State() = nil, want lazy initialization")
	}
	if flags := run.State().Flags(); flags == nil || len(flags) != 0 {
		t.Fatalf("initial Flags() = %#v, want an empty non-nil slice", flags)
	}
	if run.State().Flagged() {
		t.Fatal("new run is flagged")
	}
}

func TestRunStateFlags(t *testing.T) {
	t.Parallel()

	run := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	state := run.State()
	state.AddFlags("role_marker", "", "instruction_override", "role_marker")
	if got := state.Flags(); len(got) != 2 || got[0] != "instruction_override" || got[1] != "role_marker" {
		t.Fatalf("Flags() = %v, want [instruction_override role_marker]", got)
	}
	if !state.Flagged() {
		t.Fatal("Flagged() = false after AddFlags")
	}
	state.AddFlags("url")
	copy := state.Flags()
	copy[0] = "tampered"
	if got := state.Flags(); got[0] == "tampered" {
		t.Fatal("Flags() exposed internal storage")
	}
	if got := state.Flags(); len(got) != 3 || got[0] != "instruction_override" || got[1] != "role_marker" || got[2] != "url" {
		t.Fatalf("Flags() after second add = %v", got)
	}
}

func TestRunStateConcurrentFlags(t *testing.T) {
	t.Parallel()

	state := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))).State()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.AddFlags("role_marker", "instruction_override")
			_ = state.Flagged()
			_ = state.Flags()
		}()
	}
	wg.Wait()
	if got := state.Flags(); len(got) != 2 {
		t.Fatalf("concurrent Flags() = %v, want two unique codes", got)
	}
}

func TestRunStateProposalSlot(t *testing.T) {
	t.Parallel()

	state := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))).State()
	if state.MarkProposalUsed("") {
		t.Fatal("MarkProposalUsed(\"\") = true, want false")
	}
	if got := state.ProposalKey(); got != "" {
		t.Fatalf("ProposalKey() = %q after empty key, want empty", got)
	}
	if !state.MarkProposalUsed("key-1") {
		t.Fatal("MarkProposalUsed(key-1) = false, want true")
	}
	if !state.MarkProposalUsed("key-1") {
		t.Fatal("MarkProposalUsed(same key) = false, want true")
	}
	if state.MarkProposalUsed("key-2") {
		t.Fatal("MarkProposalUsed(different key) = true, want false")
	}
	if got := state.ProposalKey(); got != "key-1" {
		t.Fatalf("ProposalKey() = %q, want key-1", got)
	}
}

func TestRunStateProposalGuard(t *testing.T) {
	t.Parallel()

	state := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))).State()
	if err := state.ProposalGuard(); err != nil {
		t.Fatalf("ProposalGuard() on a clean run = %v, want nil", err)
	}
	state.AddFlags("instruction_override")
	err := state.ProposalGuard()
	if err == nil {
		t.Fatal("ProposalGuard() on a flagged run = nil, want injection_flagged")
	}
	coded, ok := err.(*tools.CodedError)
	if !ok {
		t.Fatalf("ProposalGuard() error is %T, want *tools.CodedError", err)
	}
	if coded.Code != tools.CodeInjectionFlagged {
		t.Fatalf("ProposalGuard() code = %q, want %q", coded.Code, tools.CodeInjectionFlagged)
	}
	if coded.Message == "" {
		t.Fatal("ProposalGuard() has an empty safe message")
	}
}

func TestRunStateEscalation(t *testing.T) {
	t.Parallel()

	state := tools.NewRunContext(testRunID, testCaseID, nil, clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))).State()
	if got := state.Escalation(); got != nil {
		t.Fatalf("initial Escalation() = %+v, want nil", got)
	}
	state.SetEscalation("   ", "ignored")
	if got := state.Escalation(); got != nil {
		t.Fatalf("blank reason set escalation: %+v", got)
	}
	state.SetEscalation("  policy_conflict  ", "summary text")
	got := state.Escalation()
	if got == nil {
		t.Fatal("Escalation() = nil after SetEscalation")
	}
	if got.Reason != "policy_conflict" || got.Summary != "summary text" {
		t.Fatalf("Escalation() = %+v", got)
	}
	got.Reason = "tampered"
	if again := state.Escalation(); again.Reason != "policy_conflict" {
		t.Fatal("Escalation() exposed internal state")
	}
	state.SetEscalation("later", "later summary")
	if again := state.Escalation(); again.Reason != "policy_conflict" {
		t.Fatalf("terminal escalation was replaced: %+v", again)
	}
}
