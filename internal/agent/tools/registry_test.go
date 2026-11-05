package tools_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestRegistryRejectsInvalidTools(t *testing.T) {
	t.Parallel()

	var typedNil *stubTool

	tests := []struct {
		name    string
		tool    tools.Tool
		wantErr error
	}{
		{name: "nil interface", tool: nil, wantErr: tools.ErrInvalidTool},
		{name: "typed nil", tool: typedNil, wantErr: tools.ErrInvalidTool},
		{name: "empty name", tool: &stubTool{name: "", schema: emptySchema}, wantErr: tools.ErrInvalidTool},
		{name: "invalid JSON", tool: &stubTool{name: "bad_json", schema: "{not json"}, wantErr: tools.ErrInvalidSchema},
		{name: "non-object schema", tool: &stubTool{
			name:   "array_schema",
			schema: `{"type":"array","additionalProperties":false}`,
		}, wantErr: tools.ErrInvalidSchema},
		{name: "missing additionalProperties", tool: &stubTool{
			name:   "open_object",
			schema: `{"type":"object"}`,
		}, wantErr: tools.ErrInvalidSchema},
		{name: "additionalProperties true", tool: &stubTool{
			name:   "open_object",
			schema: `{"type":"object","additionalProperties":true}`,
		}, wantErr: tools.ErrInvalidSchema},
		{name: "nested object missing additionalProperties", tool: &stubTool{
			name: "nested_open_object",
			schema: `{
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"nested":{"type":"object","properties":{"id":{"type":"string"}}}
				}
			}`,
		}, wantErr: tools.ErrInvalidSchema},
		{name: "uncompilable schema", tool: &stubTool{
			name: "uncompilable",
			schema: `{
				"type":"object",
				"additionalProperties":false,
				"properties":{"x":{"$ref":"#/$defs/missing"}}
			}`,
		}, wantErr: tools.ErrInvalidSchema},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := tools.NewRegistry()
			err := registry.Register(tt.tool)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Register() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	t.Parallel()

	registry := tools.NewRegistry()
	if err := registry.Register(newStub("echo", emptySchema)); err != nil {
		t.Fatalf("register first tool: %v", err)
	}
	err := registry.Register(newStub("echo", emptySchema))
	if !errors.Is(err, tools.ErrDuplicateTool) {
		t.Fatalf("Register(duplicate) error = %v, want ErrDuplicateTool", err)
	}
}

func TestRegistryNamesSortedAndLookup(t *testing.T) {
	t.Parallel()

	registry := tools.NewRegistry()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if err := registry.Register(newStub(name, emptySchema)); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	names := registry.Names()
	want := []string{"alpha", "bravo", "charlie"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Names() = %v, want %v", names, want)
	}

	tool, ok := registry.Lookup("bravo")
	if !ok || tool.Name() != "bravo" {
		t.Fatalf("Lookup(bravo) = %T, %t; want the bravo tool", tool, ok)
	}
	if _, ok := registry.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) reported a tool")
	}

	// Names returns a copy; mutating it must not affect the registry.
	names[0] = "mutated"
	if got := registry.Names()[0]; got != "alpha" {
		t.Fatalf("Names() exposed internal state: first name = %q", got)
	}
}
