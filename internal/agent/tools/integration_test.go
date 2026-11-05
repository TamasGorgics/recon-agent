//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/testdb"
)

type failingTool struct{}

func (failingTool) Name() string                 { return "not_found_tool" }
func (failingTool) Description() string          { return "always not found" }
func (failingTool) InputSchema() json.RawMessage { return json.RawMessage(emptySchema) }
func (failingTool) Call(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
	return tools.Result{}, tools.NotFound("no matching record")
}

type refsTool struct {
	advance time.Duration
}

func (r refsTool) Name() string                 { return "refs_tool" }
func (r refsTool) Description() string          { return "returns evidence refs" }
func (r refsTool) InputSchema() json.RawMessage { return json.RawMessage(emptySchema) }
func (r refsTool) Call(_ context.Context, run *tools.RunContext, _ json.RawMessage) (tools.Result, error) {
	caseRef, err := tools.CaseRef(testCaseID)
	if err != nil {
		return tools.Result{}, err
	}
	ledgerRef, err := tools.LedgerTxnRef(testTxnID)
	if err != nil {
		return tools.Result{}, err
	}
	if r.advance > 0 {
		fake, ok := run.Clock.(*clock.Fake)
		if !ok {
			return tools.Result{}, errors.New("refs tool: run clock is not fake")
		}
		fake.Advance(r.advance)
	}
	return tools.Result{Data: map[string]any{"ok": true}, Refs: []string{caseRef, ledgerRef}}, nil
}

func newIntegrationRunner(t *testing.T, world *integrationWorld, recorder tools.Recorder, fake *clock.Fake, toolList ...tools.Tool) *tools.Runner {
	t.Helper()
	registry := tools.NewRegistry()
	for _, tool := range toolList {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return tools.NewRunner(registry, recorder, fake)
}

// TestIntegrationRecordsEveryCall proves every attempted call is recorded with
// its args, envelope, hash, error code, sequence, start time and duration,
// including calls rejected before the handler runs.
func TestIntegrationRecordsEveryCall(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := clock.NewFake(base)

	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.App), fake,
		newStub("validation_echo", echoSchema),
		failingTool{},
		refsTool{advance: 25 * time.Millisecond},
	)
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	type callSpec struct {
		seq      int
		tool     string
		args     json.RawMessage
		wantCode string
	}
	calls := []callSpec{
		{seq: 1, tool: "validation_echo", args: json.RawMessage(`{}`), wantCode: tools.CodeInvalidArguments},
		{seq: 2, tool: "validation_echo", args: json.RawMessage(`{"broken":`), wantCode: tools.CodeInvalidArguments},
		{seq: 3, tool: "missing_tool", args: json.RawMessage(`{}`), wantCode: tools.CodeUnknownTool},
		{seq: 4, tool: "not_found_tool", args: json.RawMessage(`{}`), wantCode: tools.CodeNotFound},
		{seq: 5, tool: "refs_tool", args: json.RawMessage(`{}`), wantCode: ""},
	}

	returned := make(map[int]json.RawMessage, len(calls))
	for _, call := range calls {
		raw, err := runner.Call(ctx, run, call.seq, call.tool, call.args)
		if err != nil {
			t.Fatalf("call seq %d: %v", call.seq, err)
		}
		returned[call.seq] = raw
	}

	rows, err := world.App.Query(ctx,
		`SELECT seq, tool_name, args, pg_typeof(args)::text,
		        result, pg_typeof(result)::text, result_hash,
		        COALESCE(error, ''), started_at, duration_ms
		   FROM tool_calls
		  WHERE agent_run_id = $1::uuid
		  ORDER BY seq`, world.RunID)
	if err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var (
			seq                   int
			toolName              string
			args, result          []byte
			argsType, resultType  string
			resultHash, errorCode string
			startedAt             time.Time
			durationMS            int
		)
		if err := rows.Scan(&seq, &toolName, &args, &argsType, &result, &resultType,
			&resultHash, &errorCode, &startedAt, &durationMS); err != nil {
			t.Fatalf("scan tool_call: %v", err)
		}
		seen++

		spec := calls[seq-1]
		if toolName != spec.tool {
			t.Fatalf("seq %d tool_name = %q, want %q", seq, toolName, spec.tool)
		}
		if errorCode != spec.wantCode {
			t.Fatalf("seq %d error = %q, want %q", seq, errorCode, spec.wantCode)
		}
		if argsType != "jsonb" {
			t.Fatalf("seq %d args type = %s, want jsonb", seq, argsType)
		}
		if resultType != "jsonb" {
			t.Fatalf("seq %d result type = %s, want jsonb", seq, resultType)
		}

		raw := returned[seq]
		if resultHash != sha256Hex(raw) {
			t.Fatalf("seq %d result_hash = %q, want sha256 of the returned envelope", seq, resultHash)
		}
		var storedEnvelope, returnedEnvelope any
		if err := json.Unmarshal(result, &storedEnvelope); err != nil {
			t.Fatalf("seq %d stored result is not JSON: %v", seq, err)
		}
		if err := json.Unmarshal(raw, &returnedEnvelope); err != nil {
			t.Fatalf("seq %d returned envelope is not JSON: %v", seq, err)
		}
		if !reflect.DeepEqual(storedEnvelope, returnedEnvelope) {
			t.Fatalf("seq %d stored result differs from returned envelope:\nstored: %s\nreturned: %s",
				seq, result, raw)
		}

		wantDuration := 0
		wantStart := base
		if seq == 5 {
			wantDuration = 25
		}
		if durationMS != wantDuration {
			t.Fatalf("seq %d duration_ms = %d, want %d", seq, durationMS, wantDuration)
		}
		if !startedAt.Equal(wantStart) {
			t.Fatalf("seq %d started_at = %v, want %v", seq, startedAt, wantStart)
		}

		if spec.wantCode == tools.CodeInvalidArguments && seq == 2 {
			var sentinel map[string]any
			if err := json.Unmarshal(args, &sentinel); err != nil {
				t.Fatalf("seq %d sentinel args are not JSON: %v", seq, err)
			}
			if sentinel["_invalid_json"] != true {
				t.Fatalf("seq %d args = %s, want the _invalid_json sentinel", seq, args)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tool_calls: %v", err)
	}
	if seen != len(calls) {
		t.Fatalf("recorded %d calls, want %d", seen, len(calls))
	}
}

// covers: FIN-11
func TestIntegrationAgentRoleIsolation(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.App), fake, integrationInfoTool{})
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	raw, err := runner.Call(ctx, run, 1, "integration_info", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want object: %s", envelope["data"], raw)
	}

	user, _ := data["user"].(string)
	if !strings.HasPrefix(user, testdb.LoginRolePrefix()) {
		t.Fatalf("tool body current_user = %q, want an ephemeral test login", user)
	}
	if superuser, _ := data["superuser"].(string); superuser != "off" {
		t.Fatalf("tool body is_superuser = %q, want off", superuser)
	}
	if member, _ := data["member"].(bool); !member {
		t.Fatalf("tool body is not a member of recon_agent_ro: %v", data)
	}
	if denied, _ := data["audit_denied"].(bool); !denied {
		t.Fatalf("tool body could read audit_log: %v", data)
	}
}

func TestIntegrationRecorderUsesAppPool(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.App), fake, newStub("echo", emptySchema))
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	if _, err := runner.Call(ctx, run, 1, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	var count int
	if err := world.App.QueryRow(ctx,
		`SELECT count(*) FROM tool_calls WHERE agent_run_id = $1::uuid`, world.RunID).Scan(&count); err != nil {
		t.Fatalf("app read tool_calls: %v", err)
	}
	if count != 1 {
		t.Fatalf("tool_calls count = %d, want 1", count)
	}

	// The agent pool must not be able to write the trace directly; the
	// recorder succeeded only because it used the app pool.
	_, err := world.Agent.Exec(ctx,
		`INSERT INTO tool_calls
		     (agent_run_id, seq, tool_name, args, result, result_hash, started_at, duration_ms)
		 VALUES ($1::uuid, 2, 'direct', '{}'::jsonb, '{}'::jsonb, 'hash', now(), 0)`, world.RunID)
	requirePgErrorCode(t, err, "42501")
}

func TestIntegrationRecordingFailureSurfaces(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	// Deliberately use the agent pool for the recorder: the insert is denied.
	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.Agent), fake, newStub("echo", emptySchema))
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	raw, err := runner.Call(ctx, run, 1, "echo", json.RawMessage(`{}`))
	if !errors.Is(err, tools.ErrRecordFailed) {
		t.Fatalf("Call() error = %v, want ErrRecordFailed", err)
	}
	if raw != nil {
		t.Fatalf("Call() returned an envelope alongside ErrRecordFailed: %s", raw)
	}

	var count int
	if err := world.App.QueryRow(ctx,
		`SELECT count(*) FROM tool_calls WHERE agent_run_id = $1::uuid`, world.RunID).Scan(&count); err != nil {
		t.Fatalf("app read tool_calls: %v", err)
	}
	if count != 0 {
		t.Fatalf("tool_calls count = %d, want 0", count)
	}
}

func TestIntegrationRefsPersistedInToolCallResult(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.App), fake, refsTool{})
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	raw, err := runner.Call(ctx, run, 1, "refs_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	wantRefs := entryStrings(t, envelope["refs"])
	if len(wantRefs) != 2 {
		t.Fatalf("returned refs = %v, want 2", wantRefs)
	}

	var stored []byte
	if err := world.App.QueryRow(ctx,
		`SELECT result FROM tool_calls WHERE agent_run_id = $1::uuid AND seq = 1`, world.RunID).Scan(&stored); err != nil {
		t.Fatalf("read stored result: %v", err)
	}
	var storedEnvelope struct {
		Refs []string `json:"refs"`
	}
	if err := json.Unmarshal(stored, &storedEnvelope); err != nil {
		t.Fatalf("stored result is not JSON: %v", err)
	}
	if strings.Join(storedEnvelope.Refs, ",") != strings.Join(wantRefs, ",") {
		t.Fatalf("stored refs = %v, want %v", storedEnvelope.Refs, wantRefs)
	}
	if got := run.RetrievedRefs(); strings.Join(got, ",") != strings.Join(wantRefs, ",") {
		t.Fatalf("RetrievedRefs() = %v, want %v", got, wantRefs)
	}
}
