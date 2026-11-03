package audit

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestCanonicalJSON covers the canonical form used by the hash chain: object
// keys sorted, no insignificant whitespace, numbers preserved.
//
// covers: FIN-31
func TestCanonicalJSON(t *testing.T) {
	in := map[string]any{
		"b": int64(2),
		"a": map[string]any{
			"z": "x",
			"m": int64(1250),
		},
		"raw": json.RawMessage("{\n  \"q\" : true,\n  \"p\" : null\n}"),
	}
	got, err := canonicalJSON(in)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	want := `{"a":{"m":1250,"z":"x"},"b":2,"raw":{"p":null,"q":true}}`
	if string(got) != want {
		t.Fatalf("canonicalJSON =\n%s\nwant\n%s", got, want)
	}
	if bytes.ContainsAny(got, " \n\t\r") {
		t.Fatalf("canonicalJSON contains whitespace: %q", got)
	}
}

// TestCanonicalJSONKeyOrderStable proves map insertion order does not change
// the hash input.
//
// covers: FIN-31
func TestCanonicalJSONKeyOrderStable(t *testing.T) {
	first := map[string]any{"beta": "b", "alpha": "a", "amount_minor": int64(999)}
	second := map[string]any{"alpha": "a", "amount_minor": int64(999), "beta": "b"}

	a, err := canonicalJSON(first)
	if err != nil {
		t.Fatalf("canonicalJSON(first): %v", err)
	}
	b, err := canonicalJSON(second)
	if err != nil {
		t.Fatalf("canonicalJSON(second): %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("canonical JSON depends on key insertion order:\n%s\n%s", a, b)
	}
}

// TestCanonicalJSONRejectsInvalidRaw makes a broken payload a hard error
// before the caller's transaction inserts anything.
//
// covers: FIN-31
func TestCanonicalJSONRejectsInvalidRaw(t *testing.T) {
	if _, err := canonicalJSON(json.RawMessage(`{"unterminated":`)); err == nil {
		t.Fatal("canonicalJSON accepted invalid raw JSON")
	}
}

// TestGenesisPrevHash pins the genesis value to 32 zero bytes.
//
// covers: FIN-31
func TestGenesisPrevHash(t *testing.T) {
	got := zeroPrevHash()
	if len(got) != 32 {
		t.Fatalf("zeroPrevHash length = %d, want 32", len(got))
	}
	if !bytes.Equal(got, make([]byte, 32)) {
		t.Fatalf("zeroPrevHash = %x, want 32 zero bytes", got)
	}
}

// TestComputeRowHashStable proves the row hash is a pure function of the row
// fields and previous hash.
//
// covers: FIN-31
func TestComputeRowHashStable(t *testing.T) {
	ts := time.Date(2026, 11, 4, 12, 0, 0, 123456000, time.UTC)
	prev := zeroPrevHash()
	trace := "trace-1"
	before := map[string]any{"amount_minor": int64(100), "id": "txn-1"}

	h1, err := computeRowHash(1, ts, "system", "matcher", "match.run", "match_run", "run-1", before, nil, &trace, prev)
	if err != nil {
		t.Fatalf("computeRowHash: %v", err)
	}
	h2, err := computeRowHash(1, ts, "system", "matcher", "match.run", "match_run", "run-1", before, nil, &trace, prev)
	if err != nil {
		t.Fatalf("computeRowHash second call: %v", err)
	}
	if !bytes.Equal(h1, h2) {
		t.Fatalf("row hash is not stable:\n%x\n%x", h1, h2)
	}
	if len(h1) != 32 {
		t.Fatalf("row hash length = %d, want 32", len(h1))
	}

	// Every input that participates in the hash must change it.
	differentPrev := bytes.Repeat([]byte{0x01}, 32)
	h3, err := computeRowHash(1, ts, "system", "matcher", "match.run", "match_run", "run-1", before, nil, &trace, differentPrev)
	if err != nil {
		t.Fatalf("computeRowHash other prev: %v", err)
	}
	if bytes.Equal(h1, h3) {
		t.Fatal("row hash ignored prev_hash")
	}
}

// TestComputeRowHashMatchesSHA256 documents the digest construction: SHA-256
// over canonical JSON.
//
// covers: FIN-31
func TestComputeRowHashMatchesSHA256(t *testing.T) {
	ts := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	prev := zeroPrevHash()
	trace := "t"
	before := map[string]any{"amount_minor": int64(5)}

	canonical, err := canonicalJSON(map[string]any{
		"seq":         int64(7),
		"ts":          formatTimestamp(ts),
		"actor_type":  "agent",
		"actor_id":    "model-x",
		"action":      "proposal.create",
		"entity_type": "proposal",
		"entity_id":   "p-1",
		"before":      before,
		"after":       nil,
		"trace_id":    &trace,
		"prev_hash":   "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	got, err := computeRowHash(7, ts, "agent", "model-x", "proposal.create", "proposal", "p-1", before, nil, &trace, prev)
	if err != nil {
		t.Fatalf("computeRowHash: %v", err)
	}
	want := sha256Sum(canonical)
	if !bytes.Equal(got, want) {
		t.Fatalf("row hash = %x, want sha256(canonical) = %x", got, want)
	}
}
