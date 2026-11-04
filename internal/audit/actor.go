package audit

import (
	"errors"
	"fmt"
	"strings"
)

// ActorType is the type half of a typed audit actor (FIN-33).
type ActorType string

const (
	// ActorSystem identifies deterministic system work (for example
	// system:matcher and system:executor).
	ActorSystem ActorType = "system"
	// ActorAgent identifies the agent runtime and its model.
	ActorAgent ActorType = "agent"
	// ActorHuman identifies a human approver or operator.
	ActorHuman ActorType = "human"
)

// ErrInvalidActor reports that a canonical actor string, or an actor id passed
// to a typed constructor, is not valid.
var ErrInvalidActor = errors.New("audit: invalid actor")

// Actor is a typed audit actor. The zero value is invalid: construct actors
// with ParseActor or one of the typed constructors.
//
// D-15 stores actors as two columns (actor_type, actor_id); the canonical
// string "type:id" is derived, never stored a second time.
type Actor struct {
	typ ActorType
	id  string
}

// ParseActor parses the canonical "type:id" form. The accepted types are
// system, agent, and human. The id must be non-empty and must not have leading
// or trailing whitespace.
func ParseActor(s string) (Actor, error) {
	if s == "" {
		return Actor{}, fmt.Errorf("%w: empty string", ErrInvalidActor)
	}
	typ, id, ok := strings.Cut(s, ":")
	if !ok {
		return Actor{}, fmt.Errorf("%w: %q has no type prefix", ErrInvalidActor, s)
	}
	return newActor(ActorType(typ), id)
}

// SystemActor returns a system actor, for example SystemActor("matcher") or
// SystemActor("executor").
func SystemActor(id string) (Actor, error) {
	return newActor(ActorSystem, id)
}

// AgentActor returns an agent actor identified by its model name.
func AgentActor(model string) (Actor, error) {
	return newActor(ActorAgent, model)
}

// HumanActor returns a human actor identified by its internal user id.
func HumanActor(id string) (Actor, error) {
	return newActor(ActorHuman, id)
}

func newActor(typ ActorType, id string) (Actor, error) {
	switch typ {
	case ActorSystem, ActorAgent, ActorHuman:
	default:
		return Actor{}, fmt.Errorf("%w: unknown type %q", ErrInvalidActor, typ)
	}
	if id == "" {
		return Actor{}, fmt.Errorf("%w: %s actor has an empty id", ErrInvalidActor, typ)
	}
	if strings.TrimSpace(id) != id {
		return Actor{}, fmt.Errorf("%w: %s actor id %q has surrounding whitespace", ErrInvalidActor, typ, id)
	}
	return Actor{typ: typ, id: id}, nil
}

// Valid reports whether the actor was constructed successfully.
func (a Actor) Valid() bool {
	switch a.typ {
	case ActorSystem, ActorAgent, ActorHuman:
	default:
		return false
	}
	return a.id != "" && strings.TrimSpace(a.id) == a.id
}

// Type returns the actor type. It is empty for the zero value.
func (a Actor) Type() ActorType { return a.typ }

// ID returns the actor id within its type. It is empty for the zero value.
func (a Actor) ID() string { return a.id }

// String returns the canonical "type:id" representation. The zero value
// renders as ":"; callers must check Valid before persisting an actor.
func (a Actor) String() string { return string(a.typ) + ":" + a.id }
