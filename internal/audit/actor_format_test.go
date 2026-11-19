// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package audit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

type actorConstructor struct {
	name string
	typ  audit.ActorType
	fn   func(string) (audit.Actor, error)
}

var actorConstructors = []actorConstructor{
	{"SystemActor", audit.ActorSystem, audit.SystemActor},
	{"AgentActor", audit.ActorAgent, audit.AgentActor},
	{"HumanActor", audit.ActorHuman, audit.HumanActor},
}

// TestActorFormatAcceptsValidIDs pins the Go half of the actor_id format
// (spec 4.2): typed constructors accept every writer literal in the codebase,
// including ids at the 128-rune boundary counted as runes, not bytes.
func TestActorFormatAcceptsValidIDs(t *testing.T) {
	valid := []struct {
		name string
		id   string
	}{
		{"system worker", "agent-loop"},
		{"human admin", "dev-admin"},
		{"agent model", "model"},
		{"dotted model", "deepseek-v4.1-flash"},
		{"slashed model", "openai/gpt-5"},
		{"email human", "approver@example.test"},
		{"short human", "U12345"},
		{"uuid human", "123e4567-e89b-12d3-a456-426614174000"},
		{"128 ascii runes", strings.Repeat("a", 128)},
		{"128 multibyte runes", strings.Repeat("é", 128)},
	}

	for _, ctor := range actorConstructors {
		for _, tc := range valid {
			t.Run(ctor.name+"/"+tc.name, func(t *testing.T) {
				actor, err := ctor.fn(tc.id)
				if err != nil {
					t.Fatalf("%s(%q): %v", ctor.name, tc.id, err)
				}
				if actor.Type() != ctor.typ {
					t.Errorf("Type() = %q, want %q", actor.Type(), ctor.typ)
				}
				if actor.ID() != tc.id {
					t.Errorf("ID() = %q, want %q", actor.ID(), tc.id)
				}
				if got, want := actor.String(), string(ctor.typ)+":"+tc.id; got != want {
					t.Errorf("String() = %q, want %q", got, want)
				}
				if !actor.Valid() {
					t.Errorf("Valid() = false for %s(%q)", ctor.name, tc.id)
				}
			})
		}
	}
}

// TestActorFormatRejectsInvalidIDs pins the Go parity rules: non-empty,
// <=128 runes, no colon, and no ASCII whitespace or control characters. The
// raw value is validated, never trimmed.
func TestActorFormatRejectsInvalidIDs(t *testing.T) {
	invalid := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"ascii space", "a b"},
		{"leading space", " a"},
		{"trailing space", "a "},
		{"tab", "a\tb"},
		{"newline", "a\nb"},
		{"carriage return", "a\rb"},
		{"vertical tab", "a\vb"},
		{"form feed", "a\fb"},
		{"NUL", "a\x00b"},
		{"unit separator", "a\x1fb"},
		{"DEL", "a\x7fb"},
		{"colon", "a:b"},
		{"129 ascii runes", strings.Repeat("a", 129)},
		{"129 multibyte runes", strings.Repeat("é", 129)},
	}

	for _, ctor := range actorConstructors {
		for _, tc := range invalid {
			t.Run(ctor.name+"/"+tc.name, func(t *testing.T) {
				actor, err := ctor.fn(tc.id)
				if err == nil {
					t.Fatalf("%s(%q) succeeded with %q, want ErrInvalidActor", ctor.name, tc.id, actor.String())
				}
				if !errors.Is(err, audit.ErrInvalidActor) {
					t.Fatalf("%s(%q) error = %v, want ErrInvalidActor", ctor.name, tc.id, err)
				}
				if actor.Valid() {
					t.Errorf("%s(%q) returned a Valid actor after error", ctor.name, tc.id)
				}
			})
		}
	}
}

// TestParseActorFormatRoundTrip proves ParseActor keeps the canonical type:id
// shape while applying the same id rules to the id half.
func TestParseActorFormatRoundTrip(t *testing.T) {
	cases := []struct {
		canonical string
		typ       audit.ActorType
		id        string
	}{
		{"system:agent-loop", audit.ActorSystem, "agent-loop"},
		{"human:dev-admin", audit.ActorHuman, "dev-admin"},
		{"agent:model", audit.ActorAgent, "model"},
		{"agent:openai/gpt-5", audit.ActorAgent, "openai/gpt-5"},
		{"system:" + strings.Repeat("a", 128), audit.ActorSystem, strings.Repeat("a", 128)},
	}

	for _, tc := range cases {
		t.Run(tc.canonical, func(t *testing.T) {
			actor, err := audit.ParseActor(tc.canonical)
			if err != nil {
				t.Fatalf("ParseActor(%q): %v", tc.canonical, err)
			}
			if actor.Type() != tc.typ {
				t.Errorf("Type() = %q, want %q", actor.Type(), tc.typ)
			}
			if actor.ID() != tc.id {
				t.Errorf("ID() = %q, want %q", actor.ID(), tc.id)
			}
			if got := actor.String(); got != tc.canonical {
				t.Errorf("String() = %q, want %q", got, tc.canonical)
			}
			if !actor.Valid() {
				t.Errorf("Valid() = false for parsed actor %q", tc.canonical)
			}
		})
	}

	for _, canonical := range []string{
		"system:a:b",
		"system:" + strings.Repeat("a", 129),
		"human:dev admin",
		"agent:model\t",
	} {
		t.Run("rejects/"+canonical, func(t *testing.T) {
			if _, err := audit.ParseActor(canonical); !errors.Is(err, audit.ErrInvalidActor) {
				t.Fatalf("ParseActor(%q) error = %v, want ErrInvalidActor", canonical, err)
			}
		})
	}
}
