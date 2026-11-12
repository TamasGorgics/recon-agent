package state_test

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// covers: FIN-24, FIN-30, FIN-33
//
// TestPropertyRandomSequences models random event sequences against the
// transition table. An event is accepted only when the production table has
// the edge, the actor type is allowed, and the reason validates. Every accepted
// event must correspond to one of the explicit spec edges, must not start from
// a terminal status, and must increment the model's version and audit counts by
// exactly one.
func TestPropertyRandomSequences(t *testing.T) {
	actors := []audit.ActorType{audit.ActorSystem, audit.ActorAgent, audit.ActorHuman}
	reasons := []string{
		"",
		" ",
		"state_changed",
		"break_resolved",
		"expired",
		"superseded by newer proposal",
		"bad\nreason",
		strings.Repeat("x", 2001),
	}

	rapid.Check(t, func(rt *rapid.T) {
		statuses := state.AllStatuses()
		current := state.StatusProposed
		version := 0
		accepted := 0

		n := rapid.IntRange(1, 50).Draw(rt, "events")
		for i := 0; i < n; i++ {
			to := rapid.SampledFrom(statuses).Draw(rt, fmt.Sprintf("to-%d", i))
			actorType := rapid.SampledFrom(actors).Draw(rt, fmt.Sprintf("actor-%d", i))
			reason := rapid.SampledFrom(reasons).Draw(rt, fmt.Sprintf("reason-%d", i))
			actor, err := actorFor(actorType)
			if err != nil {
				rt.Fatalf("construct actor %q: %v", actorType, err)
			}

			from := current
			edge, legal := state.Lookup(from, to)
			acceptedEvent := legal &&
				from != to &&
				!state.Terminal(from) &&
				edge.AllowsActor(actor.Type()) &&
				edge.ValidateReason(reason) == nil

			if acceptedEvent {
				want, ok := findSpecEdge(from, to)
				if !ok {
					rt.Fatalf("accepted non-spec edge %s -> %s", from, to)
				}
				if want.actor != actor.Type() {
					rt.Fatalf("accepted %s -> %s with actor %s, spec allows %s",
						from, to, actor.Type(), want.actor)
				}
				if state.Terminal(from) {
					rt.Fatalf("accepted transition from terminal status %s", from)
				}
				current = to
				version++
				accepted++
				continue
			}

			if version != accepted {
				rt.Fatalf("version %d != accepted count %d", version, accepted)
			}
			if current != from {
				rt.Fatalf("rejected event changed status from %s to %s", from, current)
			}
		}

		if version != accepted {
			rt.Fatalf("final version %d != accepted count %d", version, accepted)
		}
		if !current.Valid() {
			rt.Fatalf("model reached unknown status %q", current)
		}
		if current != state.StatusProposed && accepted == 0 {
			rt.Fatalf("model reached %s with no accepted transition", current)
		}
	})
}

func actorFor(t audit.ActorType) (audit.Actor, error) {
	switch t {
	case audit.ActorSystem:
		return audit.SystemActor("property")
	case audit.ActorAgent:
		return audit.AgentActor("property")
	case audit.ActorHuman:
		return audit.HumanActor("property")
	default:
		return audit.Actor{}, fmt.Errorf("unknown actor type %q", t)
	}
}
