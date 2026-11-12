package state_test

import (
	"slices"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// specEdge is the explicit, hand-written expectation for one of the 17 legal
// edges. It deliberately does not derive from state.Edges so a production table
// edit cannot silently update the test.
type specEdge struct {
	from           state.Status
	to             state.Status
	actor          audit.ActorType
	reasonRequired bool
	reasonEnum     []string
}

var applyFailedEnum = []string{"state_changed", "break_resolved", "expired"}

var specEdges = []specEdge{
	{state.StatusProposed, state.StatusPendingApproval, audit.ActorSystem, false, nil},
	{state.StatusProposed, state.StatusAutoApproved, audit.ActorSystem, false, nil},
	{state.StatusProposed, state.StatusExpired, audit.ActorSystem, false, nil},
	{state.StatusProposed, state.StatusSuperseded, audit.ActorSystem, true, nil},
	{state.StatusPendingApproval, state.StatusApproved, audit.ActorHuman, false, nil},
	{state.StatusPendingApproval, state.StatusRejected, audit.ActorHuman, true, nil},
	{state.StatusPendingApproval, state.StatusExpired, audit.ActorSystem, false, nil},
	{state.StatusPendingApproval, state.StatusSuperseded, audit.ActorSystem, true, nil},
	{state.StatusApproved, state.StatusApplied, audit.ActorSystem, false, nil},
	{state.StatusApproved, state.StatusApplyFailed, audit.ActorSystem, true, applyFailedEnum},
	{state.StatusApproved, state.StatusSuperseded, audit.ActorSystem, true, nil},
	{state.StatusAutoApproved, state.StatusApplied, audit.ActorSystem, false, nil},
	{state.StatusAutoApproved, state.StatusApplyFailed, audit.ActorSystem, true, applyFailedEnum},
	{state.StatusAutoApproved, state.StatusSuperseded, audit.ActorSystem, true, nil},
	{state.StatusApplyFailed, state.StatusApproved, audit.ActorSystem, false, nil},
	{state.StatusApplyFailed, state.StatusAutoApproved, audit.ActorSystem, false, nil},
	{state.StatusApplyFailed, state.StatusSuperseded, audit.ActorSystem, true, nil},
}

func findSpecEdge(from, to state.Status) (specEdge, bool) {
	for _, edge := range specEdges {
		if edge.from == from && edge.to == to {
			return edge, true
		}
	}
	return specEdge{}, false
}

func TestTransitionTableExhaustive(t *testing.T) {
	edges := state.Edges()
	if len(edges) != 17 {
		t.Fatalf("len(Edges()) = %d, want exactly 17", len(edges))
	}
	if len(specEdges) != 17 {
		t.Fatalf("test expectation has %d edges, want 17", len(specEdges))
	}

	for i, want := range specEdges {
		got := edges[i]
		if got.From != want.from || got.To != want.to {
			t.Errorf("edge %d = %s -> %s, want %s -> %s",
				i+1, got.From, got.To, want.from, want.to)
		}
		if got.Actor != want.actor {
			t.Errorf("edge %d actor = %q, want %q", i+1, got.Actor, want.actor)
		}
		if got.ReasonRequired != want.reasonRequired {
			t.Errorf("edge %d reasonRequired = %t, want %t",
				i+1, got.ReasonRequired, want.reasonRequired)
		}
		if !slices.Equal(got.ReasonEnum, want.reasonEnum) {
			t.Errorf("edge %d reasonEnum = %v, want %v", i+1, got.ReasonEnum, want.reasonEnum)
		}
		if got.Purpose == "" {
			t.Errorf("edge %d has an empty purpose", i+1)
		}
		if _, ok := state.Lookup(got.From, got.To); !ok {
			t.Errorf("Lookup(%s, %s) is missing", got.From, got.To)
		}
	}

	// The 9x9 matrix: exactly the 17 explicit pairs are legal.
	legal := make(map[[2]state.Status]bool, len(specEdges))
	for _, edge := range specEdges {
		legal[[2]state.Status{edge.from, edge.to}] = true
	}
	legalCount := 0
	for _, from := range state.AllStatuses() {
		for _, to := range state.AllStatuses() {
			_, ok := state.Lookup(from, to)
			want := legal[[2]state.Status{from, to}]
			if ok != want {
				t.Errorf("Lookup(%s, %s) = %t, want %t", from, to, ok, want)
			}
			if ok {
				legalCount++
			}
		}
	}
	if legalCount != 17 {
		t.Fatalf("legal matrix count = %d, want 17", legalCount)
	}

	// The explicitly dangerous illegal pairs are absent.
	for _, pair := range [][2]state.Status{
		{state.StatusProposed, state.StatusApplied},
		{state.StatusProposed, state.StatusRejected},
		{state.StatusPendingApproval, state.StatusApplied},
		{state.StatusPendingApproval, state.StatusAutoApproved},
		{state.StatusApproved, state.StatusExpired},
		{state.StatusAutoApproved, state.StatusApproved},
		{state.StatusApplyFailed, state.StatusApplied},
	} {
		if _, ok := state.Lookup(pair[0], pair[1]); ok {
			t.Errorf("Lookup(%s, %s) returned an edge, want none", pair[0], pair[1])
		}
	}
	for _, s := range state.AllStatuses() {
		if _, ok := state.Lookup(s, s); ok {
			t.Errorf("self-transition %s -> %s is in the table", s, s)
		}
		if state.Terminal(s) {
			for _, to := range state.AllStatuses() {
				if _, ok := state.Lookup(s, to); ok {
					t.Errorf("terminal %s has outgoing edge to %s", s, to)
				}
			}
		}
	}

	// Edges is deterministic and returns a copy.
	again := state.Edges()
	if !slices.EqualFunc(edges, again, func(a, b state.Edge) bool {
		return a.From == b.From && a.To == b.To && a.Actor == b.Actor &&
			a.ReasonRequired == b.ReasonRequired && slices.Equal(a.ReasonEnum, b.ReasonEnum) &&
			a.Purpose == b.Purpose
	}) {
		t.Fatal("Edges() is not deterministic")
	}
	again[0].From = "mutated"
	if state.Edges()[0].From != state.StatusProposed {
		t.Fatal("Edges() returned the package table")
	}
}

// covers: FIN-33
func TestActorRules(t *testing.T) {
	humanEdges := 0
	systemEdges := 0
	for _, edge := range state.Edges() {
		if edge.AllowsActor(audit.ActorAgent) {
			t.Errorf("agent allowed on %s -> %s", edge.From, edge.To)
		}
		switch edge.Actor {
		case audit.ActorHuman:
			humanEdges++
			if edge.From != state.StatusPendingApproval ||
				(edge.To != state.StatusApproved && edge.To != state.StatusRejected) {
				t.Errorf("human actor on unexpected edge %s -> %s", edge.From, edge.To)
			}
		case audit.ActorSystem:
			systemEdges++
		default:
			t.Errorf("edge %s -> %s has unknown actor %q", edge.From, edge.To, edge.Actor)
		}
		if edge.AllowsActor(audit.ActorSystem) != (edge.Actor == audit.ActorSystem) {
			t.Errorf("AllowsActor(system) on %s -> %s = %t, want %t",
				edge.From, edge.To, edge.AllowsActor(audit.ActorSystem), edge.Actor == audit.ActorSystem)
		}
		if edge.AllowsActor(audit.ActorHuman) != (edge.Actor == audit.ActorHuman) {
			t.Errorf("AllowsActor(human) on %s -> %s = %t, want %t",
				edge.From, edge.To, edge.AllowsActor(audit.ActorHuman), edge.Actor == audit.ActorHuman)
		}
	}
	if humanEdges != 2 {
		t.Fatalf("human edges = %d, want 2", humanEdges)
	}
	if systemEdges != 15 {
		t.Fatalf("system edges = %d, want 15", systemEdges)
	}
}
