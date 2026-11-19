package state

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// actorClass is the class of a parsed actor string. Agent actors are parsed so
// they can be reported as ErrActorNotAllowed rather than malformed.
type actorClass int

const (
	actorClassHuman actorClass = iota
	actorClassSystem
	actorClassAgent
)

// maxActorIDBytes bounds the actor suffix. The whole canonical string is
// persisted in the audit row, so an unbounded id would let a caller bloat the
// chain.
const maxActorIDBytes = 128

// parseActor validates the canonical "human:<id>" / "system:<component>" form.
// The prefix is case-sensitive; the suffix must be non-empty, at most 128
// bytes, and contain no whitespace or control characters. Malformed input,
// including a malformed agent actor, is ErrInvalidRequest. A well-formed
// agent actor is returned with actorClassAgent and rejected later, after the
// edge lookup, by the actor rule (ErrActorNotAllowed).
func parseActor(raw string) (audit.Actor, actorClass, error) {
	prefix, id, ok := strings.Cut(raw, ":")
	if !ok || prefix == "" {
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor %q has no type prefix", ErrInvalidRequest, raw)
	}
	var class actorClass
	switch prefix {
	case "human":
		class = actorClassHuman
	case "system":
		class = actorClassSystem
	case "agent":
		class = actorClassAgent
	default:
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor %q has an unknown type prefix", ErrInvalidRequest, raw)
	}
	if id == "" {
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor %q has an empty id", ErrInvalidRequest, raw)
	}
	if len(id) > maxActorIDBytes {
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor id exceeds %d bytes", ErrInvalidRequest, maxActorIDBytes)
	}
	if !utf8.ValidString(id) {
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor %q is not valid UTF-8", ErrInvalidRequest, raw)
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return audit.Actor{}, actorClassHuman,
				fmt.Errorf("%w: actor id %q contains whitespace or a control character",
					ErrInvalidRequest, id)
		}
	}

	var (
		actor audit.Actor
		err   error
	)
	switch class {
	case actorClassHuman:
		actor, err = audit.HumanActor(id)
	case actorClassSystem:
		actor, err = audit.SystemActor(id)
	case actorClassAgent:
		actor, err = audit.AgentActor(id)
	}
	if err != nil {
		return audit.Actor{}, actorClassHuman,
			fmt.Errorf("%w: actor %q: %v", ErrInvalidRequest, raw, err)
	}
	return actor, class, nil
}

func (c actorClass) String() string {
	switch c {
	case actorClassHuman:
		return "human"
	case actorClassSystem:
		return "system"
	case actorClassAgent:
		return "agent"
	default:
		return "unknown"
	}
}
