package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// TestRegisterPart2 proves the part-2 composer registers exactly the four
// M2-06 read tools without touching the M2-05 catalog.
func TestRegisterPart2(t *testing.T) {
	t.Parallel()

	registry := tools.NewRegistry()
	if err := tools.RegisterPart2(registry, tools.Deps{}); err != nil {
		t.Fatalf("RegisterPart2: %v", err)
	}

	want := []string{
		"get_fee_schedule",
		"get_fx_rate",
		"get_history",
		"get_settlement_batch",
	}
	if got := registry.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for _, name := range want {
		tool, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) = false", name)
		}
		if tool.Description() == "" {
			t.Errorf("%s has an empty description", name)
		}
		if len(tool.InputSchema()) == 0 {
			t.Errorf("%s has an empty schema", name)
		}
	}

	if err := tools.RegisterPart2(registry, tools.Deps{}); err == nil {
		t.Fatal("RegisterPart2 accepted a duplicate registration")
	}
}

// TestRegisterPart2RejectsInvalidArguments covers the schema and handler-level
// validation paths that must fail before any database query.
func TestRegisterPart2RejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	registry := tools.NewRegistry()
	if err := tools.RegisterPart2(registry, tools.Deps{}); err != nil {
		t.Fatalf("RegisterPart2: %v", err)
	}
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	recorder := &fakeRecorder{}
	runner := tools.NewRunner(registry, recorder, fake)
	run := tools.NewRunContext(testRunID, testCaseID, nil, fake)

	calls := []struct {
		name string
		tool string
		args string
	}{
		{
			name: "batch missing id",
			tool: "get_settlement_batch",
			args: `{}`,
		},
		{
			name: "batch malformed id",
			tool: "get_settlement_batch",
			args: `{"batch_id":"not-a-uuid"}`,
		},
		{
			name: "batch unknown field",
			tool: "get_settlement_batch",
			args: `{"batch_id":"44444444-4444-4444-8444-444444444444","extra":true}`,
		},
		{
			name: "fx base equals quote",
			tool: "get_fx_rate",
			args: `{"base":"USD","quote":"USD","date":"2026-01-02"}`,
		},
		{
			name: "fx malformed date",
			tool: "get_fx_rate",
			args: `{"base":"USD","quote":"EUR","date":"02-01-2026"}`,
		},
		{
			name: "fee impossible date",
			tool: "get_fee_schedule",
			args: `{"source":"psp_a","kind":"card","currency":"USD","date":"2026-02-30"}`,
		},
		{
			name: "fee missing currency",
			tool: "get_fee_schedule",
			args: `{"source":"psp_a","kind":"card","date":"2026-01-02"}`,
		},
		{
			name: "history upper-case signature",
			tool: "get_history",
			args: `{"signature":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`,
		},
	}

	for index, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			raw, err := runner.Call(context.Background(), run, index+1, call.tool, json.RawMessage(call.args))
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			envelope := decodeEnvelope(t, raw)
			if code := envelopeErrorCode(t, envelope); code != tools.CodeInvalidArguments {
				t.Fatalf("error code = %q, want %q: %s", code, tools.CodeInvalidArguments, raw)
			}
			refs := entryStrings(t, envelope["refs"])
			if len(refs) != 0 {
				t.Fatalf("invalid call returned refs: %v", refs)
			}
		})
	}
}
