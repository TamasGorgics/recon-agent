//go:build integration

package tools_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// TestIntegrationRunnerRecordsCanonicalResultHash proves the production runner
// path writes both the legacy result_hash (exact pre-insert envelope bytes) and
// the canonical result hash, and that the canonical hash is reproducible from
// the stored JSONB value.
func TestIntegrationRunnerRecordsCanonicalResultHash(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	tool := &stubTool{
		name:   "canonical_echo",
		schema: emptySchema,
		call: func(context.Context, *tools.RunContext, json.RawMessage) (tools.Result, error) {
			return tools.Result{Data: map[string]any{
				"z": int64(1),
				"a": map[string]any{
					"nested":  []any{int64(3), int64(1), int64(2)},
					"nil":     nil,
					"empty":   "",
					"unicode": "café ☃",
					"html":    "<&>",
					"big":     int64(9007199254740993),
				},
			}}, nil
		},
	}
	runner := newIntegrationRunner(t, world, tools.NewDBRecorder(world.App), fake, tool)
	run := tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake)

	raw, err := runner.Call(ctx, run, 1, "canonical_echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var (
		storedResult  []byte
		resultHash    string
		canonicalHash string
	)
	if err := world.App.QueryRow(ctx,
		`SELECT result, result_hash, result_canonical_hash
		   FROM tool_calls
		  WHERE agent_run_id = $1::uuid AND seq = 1`, world.RunID).
		Scan(&storedResult, &resultHash, &canonicalHash); err != nil {
		t.Fatalf("read recorded tool call: %v", err)
	}

	if resultHash != sha256Hex(raw) {
		t.Fatalf("result_hash = %q, want sha256 of the returned envelope", resultHash)
	}
	if !isLowerHex64(canonicalHash) {
		t.Fatalf("result_canonical_hash = %q, want 64 lowercase hex characters", canonicalHash)
	}
	recomputed, err := tools.CanonicalJSONHash(storedResult)
	if err != nil {
		t.Fatalf("CanonicalJSONHash(stored): %v", err)
	}
	if recomputed != canonicalHash {
		t.Fatalf("canonical hash after round-trip = %q, stored = %q", recomputed, canonicalHash)
	}
	canonical, err := tools.CanonicalJSON(storedResult)
	if err != nil {
		t.Fatalf("CanonicalJSON(stored): %v", err)
	}
	if strings.Contains(string(canonical), `\u003c`) || strings.Contains(string(canonical), `\u0026`) {
		t.Fatalf("canonical JSON HTML-escaped stored data: %s", canonical)
	}
	if !strings.Contains(string(canonical), "<&>") {
		t.Fatalf("canonical JSON lost raw HTML characters: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"unicode":"café ☃"`) {
		t.Fatalf("canonical JSON lost UTF-8 text: %s", canonical)
	}
}

// TestIntegrationDBRecorderCanonicalHashStableForJSONBNormalizedNumbers proves
// the recorded canonical hash is computed from PostgreSQL's JSONB text, not the
// pre-insert bytes: JSONB expands exponents (1e2 -> 100) and keeps key order
// independent, yet a later replay that re-canonicalises the stored value gets
// exactly the stored hash.
func TestIntegrationDBRecorderCanonicalHashStableForJSONBNormalizedNumbers(t *testing.T) {
	ctx := context.Background()
	world := newIntegrationWorld(t)
	recorder := tools.NewDBRecorder(world.App)

	envelope := json.RawMessage(`{"data":{"z":1,"a":{"nested":[3,1,2],"nil":null,"empty":"","unicode":"café ☃","html":"<&>","big":9007199254740993,"exp":1e2,"dec":1.50}},"refs":[],"untrusted_fields":[],"truncated":false}`)
	call := tools.ToolCall{
		RunID:      world.RunID,
		Seq:        1,
		ToolName:   "canonical_numbers",
		Args:       json.RawMessage(`{}`),
		Result:     envelope,
		ResultHash: sha256Hex(envelope),
		StartedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		DurationMS: 0,
	}
	if err := recorder.RecordToolCall(ctx, call); err != nil {
		t.Fatalf("RecordToolCall: %v", err)
	}

	var (
		storedResult  []byte
		resultHash    string
		canonicalHash string
	)
	if err := world.App.QueryRow(ctx,
		`SELECT result, result_hash, result_canonical_hash
		   FROM tool_calls
		  WHERE agent_run_id = $1::uuid AND seq = 1`, world.RunID).
		Scan(&storedResult, &resultHash, &canonicalHash); err != nil {
		t.Fatalf("read recorded tool call: %v", err)
	}
	if resultHash != sha256Hex(envelope) {
		t.Fatalf("result_hash = %q, want sha256 of the pre-insert envelope", resultHash)
	}
	if !strings.Contains(string(storedResult), `"exp": 100`) {
		t.Fatalf("JSONB did not normalise 1e2 to 100: %s", storedResult)
	}
	if strings.Contains(string(storedResult), `"exp": 1e2`) {
		t.Fatalf("JSONB kept the exponent form: %s", storedResult)
	}

	recomputed, err := tools.CanonicalJSONHash(storedResult)
	if err != nil {
		t.Fatalf("CanonicalJSONHash(stored): %v", err)
	}
	if recomputed != canonicalHash {
		t.Fatalf("canonical hash after round-trip = %q, stored = %q", recomputed, canonicalHash)
	}
	canonical, err := tools.CanonicalJSON(storedResult)
	if err != nil {
		t.Fatalf("CanonicalJSON(stored): %v", err)
	}
	if strings.Contains(string(canonical), "1e2") {
		t.Fatalf("canonical JSON kept the pre-JSONB exponent form: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"dec":1.50`) {
		t.Fatalf("canonical JSON lost the JSONB decimal representation: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"nil":null`) || !strings.Contains(string(canonical), `"empty":""`) {
		t.Fatalf("canonical JSON did not keep null and empty string distinct: %s", canonical)
	}
}

func isLowerHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
