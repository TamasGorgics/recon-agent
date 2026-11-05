package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// newPart1UnitRunner registers the four part-1 tools without a database. It is
// used for argument-validation tests, which must fail before any query runs.
func newPart1UnitRunner(t *testing.T) (*tools.Runner, *fakeRecorder, *tools.RunContext) {
	t.Helper()
	registry := tools.NewRegistry()
	if err := tools.RegisterPart1(registry, tools.Deps{}); err != nil {
		t.Fatalf("RegisterPart1: %v", err)
	}
	recorder := &fakeRecorder{}
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	run := tools.NewRunContext(testRunID, testCaseID, nil, fake)
	return tools.NewRunner(registry, recorder, fake), recorder, run
}

func TestPart1Registration(t *testing.T) {
	t.Parallel()

	registry := tools.NewRegistry()
	if err := tools.RegisterPart1(registry, tools.Deps{}); err != nil {
		t.Fatalf("RegisterPart1: %v", err)
	}
	want := []string{
		"get_case",
		"lookup_external_record",
		"lookup_ledger_txn",
		"search_external_records",
	}
	if got := registry.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Names() = %v, want %v", got, want)
	}

	if err := tools.RegisterPart1(registry, tools.Deps{}); !errors.Is(err, tools.ErrDuplicateTool) {
		t.Fatalf("second RegisterPart1 error = %v, want ErrDuplicateTool", err)
	}
	if err := tools.RegisterPart1(nil, tools.Deps{}); err == nil {
		t.Fatal("RegisterPart1(nil) = nil, want an error")
	}
}

func TestPart1ArgumentValidation(t *testing.T) {
	t.Parallel()

	runner, recorder, run := newPart1UnitRunner(t)
	cases := []struct {
		name string
		tool string
		args string
	}{
		{name: "get_case missing id", tool: "get_case", args: `{}`},
		{name: "get_case malformed id", tool: "get_case", args: `{"case_id":"not-a-uuid"}`},
		{name: "lookup ledger neither selector", tool: "lookup_ledger_txn", args: `{}`},
		{
			name: "lookup ledger both selectors",
			tool: "lookup_ledger_txn",
			args: `{"txn_id":"` + testTxnID + `","external_ref":"REF-1"}`,
		},
		{name: "lookup ledger malformed txn id", tool: "lookup_ledger_txn", args: `{"txn_id":"3333"}`},
		{name: "lookup ledger empty external ref", tool: "lookup_ledger_txn", args: `{"external_ref":""}`},
		{name: "lookup external missing record", tool: "lookup_external_record", args: `{"source":"psp_a"}`},
		{name: "lookup external empty record", tool: "lookup_external_record", args: `{"source":"psp_a","record_id":""}`},
		{name: "lookup external malformed source", tool: "lookup_external_record", args: `{"source":"PSP-A","record_id":"r1"}`},
		{name: "search missing amount", tool: "search_external_records", args: `{"currency":"USD","from":"2026-01-01","to":"2026-01-02"}`},
		{name: "search unknown field", tool: "search_external_records", args: `{"amount_minor":1,"currency":"USD","from":"2026-01-01","to":"2026-01-02","extra":1}`},
		{name: "search malformed currency", tool: "search_external_records", args: `{"amount_minor":1,"currency":"usd","from":"2026-01-01","to":"2026-01-02"}`},
		{name: "search malformed date", tool: "search_external_records", args: `{"amount_minor":1,"currency":"USD","from":"2026-1-1","to":"2026-01-02"}`},
		{name: "search reference too long", tool: "search_external_records", args: `{"amount_minor":1,"currency":"USD","from":"2026-01-01","to":"2026-01-02","reference":"` + strings.Repeat("x", 129) + `"}`},
	}

	for i, tc := range cases {
		raw, err := runner.Call(context.Background(), run, i+1, tc.tool, json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s: Call: %v", tc.name, err)
		}
		envelope := decodeEnvelope(t, raw)
		if got := envelopeErrorCode(t, envelope); got != tools.CodeInvalidArguments {
			t.Fatalf("%s: error code = %q, want %q (%s)", tc.name, got, tools.CodeInvalidArguments, raw)
		}
	}
	if got, want := len(recorder.all()), len(cases); got != want {
		t.Fatalf("recorded %d invalid calls, want %d", got, want)
	}
}

// covers: FIN-05
func TestPart1AmountsMustBeInteger(t *testing.T) {
	t.Parallel()

	runner, _, run := newPart1UnitRunner(t)
	validPrefix := `"currency":"USD","from":"2026-01-01","to":"2026-01-02"`
	amounts := []string{"12.5", `"12"`, "12.0", "1e3", "null"}
	for i, amount := range amounts {
		args := json.RawMessage(`{"amount_minor":` + amount + `,` + validPrefix + `}`)
		raw, err := runner.Call(context.Background(), run, i+1, "search_external_records", args)
		if err != nil {
			t.Fatalf("amount %s: Call: %v", amount, err)
		}
		envelope := decodeEnvelope(t, raw)
		if got := envelopeErrorCode(t, envelope); got != tools.CodeInvalidArguments {
			t.Fatalf("amount %s: error code = %q, want %q (%s)", amount, got, tools.CodeInvalidArguments, raw)
		}
	}
}

// covers: FIN-42
func TestPart1SearchWindowValidation(t *testing.T) {
	t.Parallel()

	runner, _, run := newPart1UnitRunner(t)
	cases := []struct {
		name string
		args string
	}{
		{
			name: "from after to",
			args: `{"amount_minor":1,"currency":"USD","from":"2026-02-01","to":"2026-01-31"}`,
		},
		{
			name: "32-day inclusive window",
			args: `{"amount_minor":1,"currency":"USD","from":"2026-01-01","to":"2026-02-01"}`,
		},
	}
	for i, tc := range cases {
		raw, err := runner.Call(context.Background(), run, i+1, "search_external_records", json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s: Call: %v", tc.name, err)
		}
		envelope := decodeEnvelope(t, raw)
		if got := envelopeErrorCode(t, envelope); got != tools.CodeInvalidArguments {
			t.Fatalf("%s: error code = %q, want %q (%s)", tc.name, got, tools.CodeInvalidArguments, raw)
		}
	}
}
