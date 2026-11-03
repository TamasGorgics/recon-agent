package audit_test

import (
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// TestParseActor covers the four typed actor shapes from FIN-33: the
// system, agent, and human prefixes with a non-empty id. Executor is the
// system:executor instance, not a separate type.
//
// covers: FIN-33
func TestParseActor(t *testing.T) {
	cases := []struct {
		in       string
		wantType audit.ActorType
		wantID   string
	}{
		{"system:matcher", audit.ActorSystem, "matcher"},
		{"system:executor", audit.ActorSystem, "executor"},
		{"agent:deepseek-v4.1-flash", audit.ActorAgent, "deepseek-v4.1-flash"},
		{"agent:openai/gpt-5", audit.ActorAgent, "openai/gpt-5"},
		{"human:approver@example.test", audit.ActorHuman, "approver@example.test"},
		{"human:U12345", audit.ActorHuman, "U12345"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			actor, err := audit.ParseActor(tc.in)
			if err != nil {
				t.Fatalf("ParseActor(%q): %v", tc.in, err)
			}
			if actor.Type() != tc.wantType {
				t.Errorf("Type() = %q, want %q", actor.Type(), tc.wantType)
			}
			if actor.ID() != tc.wantID {
				t.Errorf("ID() = %q, want %q", actor.ID(), tc.wantID)
			}
			if got := actor.String(); got != tc.in {
				t.Errorf("String() = %q, want %q", got, tc.in)
			}
			if !actor.Valid() {
				t.Errorf("Valid() = false for parsed actor %q", tc.in)
			}
		})
	}
}

// TestParseActorRejectsInvalid pins the constructor contract: unknown
// prefixes, missing ids, empty values, and surrounding whitespace are errors.
//
// covers: FIN-33
func TestParseActorRejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		" ",
		"matcher",
		"system",
		":matcher",
		"system:",
		"system:   ",
		"unknown:x",
		"System:matcher",
		"system :matcher",
		" system:matcher",
		"system:matcher ",
		"agent:",
		"human:",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			actor, err := audit.ParseActor(in)
			if err == nil {
				t.Fatalf("ParseActor(%q) succeeded with %q, want an error", in, actor.String())
			}
			if !errors.Is(err, audit.ErrInvalidActor) {
				t.Fatalf("ParseActor(%q) error = %v, want ErrInvalidActor", in, err)
			}
		})
	}
}

// TestActorConstructors checks the typed constructors reject empty ids and
// reconstruct the canonical string.
//
// covers: FIN-33
func TestActorConstructors(t *testing.T) {
	system, err := audit.SystemActor("matcher")
	if err != nil {
		t.Fatalf("SystemActor: %v", err)
	}
	if system.String() != "system:matcher" {
		t.Fatalf("SystemActor.String() = %q", system.String())
	}

	agent, err := audit.AgentActor("deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("AgentActor: %v", err)
	}
	if agent.String() != "agent:deepseek-v4.1-flash" {
		t.Fatalf("AgentActor.String() = %q", agent.String())
	}

	human, err := audit.HumanActor("alice")
	if err != nil {
		t.Fatalf("HumanActor: %v", err)
	}
	if human.String() != "human:alice" {
		t.Fatalf("HumanActor.String() = %q", human.String())
	}

	for name, fn := range map[string]func(string) (audit.Actor, error){
		"SystemActor": audit.SystemActor,
		"AgentActor":  audit.AgentActor,
		"HumanActor":  audit.HumanActor,
	} {
		t.Run(name+" empty", func(t *testing.T) {
			if _, err := fn(""); !errors.Is(err, audit.ErrInvalidActor) {
				t.Fatalf("%s(\"\") error = %v, want ErrInvalidActor", name, err)
			}
		})
	}
}

// TestActorZeroValueInvalid ensures an unset Actor can never be persisted by
// accident: Append must reject the zero value.
//
// covers: FIN-33
func TestActorZeroValueInvalid(t *testing.T) {
	var actor audit.Actor
	if actor.Valid() {
		t.Fatal("zero Actor is Valid, want invalid")
	}
}
