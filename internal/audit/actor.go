package audit

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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

// maxActorIDRunes is the maximum length of the id half of an actor, counted in
// runes to match the database CHECK.
const maxActorIDRunes = 128

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
// system, agent, and human. The id must be 1-128 runes and must not contain a
// colon, ASCII whitespace, or an ASCII control character.
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
	if problem := actorIDProblem(id); problem != "" {
		return Actor{}, fmt.Errorf("%w: %s actor id %q %s", ErrInvalidActor, typ, id, problem)
	}
	return Actor{typ: typ, id: id}, nil
}

// actorIDProblem describes the first format violation in id, or returns an
// empty string when id is valid. The rules are ASCII-only by design: the
// database CHECK is the authoritative Unicode backstop (spec 4.2).
func actorIDProblem(id string) string {
	if utf8.RuneCountInString(id) > maxActorIDRunes {
		return fmt.Sprintf("is longer than %d runes", maxActorIDRunes)
	}
	if strings.ContainsRune(id, ':') {
		return "contains a colon"
	}
	for _, r := range id {
		if isASCIISpaceOrControl(r) {
			return "contains ASCII whitespace or a control character"
		}
	}
	return ""
}

// isASCIISpaceOrControl reports whether r is ASCII whitespace (including the
// space character) or an ASCII control character (U+0000-U+001F or U+007F).
func isASCIISpaceOrControl(r rune) bool {
	return r == ' ' || r <= 0x1f || r == 0x7f
}

// Valid reports whether the actor was constructed successfully and its id
// satisfies the same format rules as the typed constructors.
func (a Actor) Valid() bool {
	switch a.typ {
	case ActorSystem, ActorAgent, ActorHuman:
	default:
		return false
	}
	return a.id != "" && actorIDProblem(a.id) == ""
}

// Type returns the actor type. It is empty for the zero value.
func (a Actor) Type() ActorType { return a.typ }

// ID returns the actor id within its type. It is empty for the zero value.
func (a Actor) ID() string { return a.id }

// String returns the canonical "type:id" representation. The zero value
// renders as ":"; callers must check Valid before persisting an actor.
func (a Actor) String() string { return string(a.typ) + ":" + a.id }
