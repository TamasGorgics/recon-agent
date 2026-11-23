package tools_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestCanonicalJSONSortsKeysAndPreservesValues(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"z": 1,
		"a": {
			"m": "<&>",
			"b": [3, 1, 2],
			"nil": null,
			"empty": "",
			"unicode": "café ☃"
		},
		"num": 1e2,
		"big": 9007199254740993
	}`)
	got, err := tools.CanonicalJSON(input)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	want := `{"a":{"b":[3,1,2],"empty":"","m":"<&>","nil":null,"unicode":"café ☃"},"big":9007199254740993,"num":1e2,"z":1}`
	if string(got) != want {
		t.Fatalf("CanonicalJSON =\n%s\nwant\n%s", got, want)
	}
	if bytes.ContainsAny(got, "\n\r\t") {
		t.Fatalf("canonical JSON contains insignificant whitespace: %q", got)
	}
	if bytes.Contains(got, []byte(`\u003c`)) || bytes.Contains(got, []byte(`\u003e`)) ||
		bytes.Contains(got, []byte(`\u0026`)) {
		t.Fatalf("canonical JSON HTML-escaped a string: %s", got)
	}
}

func TestCanonicalJSONNullAndEmptyAreDistinct(t *testing.T) {
	t.Parallel()

	nullJSON, err := tools.CanonicalJSON([]byte(`{"value":null}`))
	if err != nil {
		t.Fatalf("CanonicalJSON(null): %v", err)
	}
	emptyJSON, err := tools.CanonicalJSON([]byte(`{"value":""}`))
	if err != nil {
		t.Fatalf("CanonicalJSON(empty): %v", err)
	}
	if string(nullJSON) != `{"value":null}` {
		t.Fatalf("null canonical = %s", nullJSON)
	}
	if string(emptyJSON) != `{"value":""}` {
		t.Fatalf("empty canonical = %s", emptyJSON)
	}
	nullHash, err := tools.CanonicalJSONHash([]byte(`{"value":null}`))
	if err != nil {
		t.Fatalf("CanonicalJSONHash(null): %v", err)
	}
	emptyHash, err := tools.CanonicalJSONHash([]byte(`{"value":""}`))
	if err != nil {
		t.Fatalf("CanonicalJSONHash(empty): %v", err)
	}
	if nullHash == emptyHash {
		t.Fatalf("null and empty string hash to the same value: %s", nullHash)
	}
}

func TestCanonicalJSONKeyOrderAndWhitespaceDoNotChangeHash(t *testing.T) {
	t.Parallel()

	first := []byte(`{"b":2,"a":{"z":true,"m":1250},"raw":[1,2,3]}`)
	second := []byte("{\n  \"raw\" : [1, 2, 3],\n  \"a\" : {\"m\" : 1250, \"z\" : true},\n  \"b\" : 2\n}")
	firstCanonical, err := tools.CanonicalJSON(first)
	if err != nil {
		t.Fatalf("CanonicalJSON(first): %v", err)
	}
	secondCanonical, err := tools.CanonicalJSON(second)
	if err != nil {
		t.Fatalf("CanonicalJSON(second): %v", err)
	}
	if !bytes.Equal(firstCanonical, secondCanonical) {
		t.Fatalf("canonical JSON depends on key order or whitespace:\n%s\n%s",
			firstCanonical, secondCanonical)
	}
	firstHash, err := tools.CanonicalJSONHash(first)
	if err != nil {
		t.Fatalf("CanonicalJSONHash(first): %v", err)
	}
	secondHash, err := tools.CanonicalJSONHash(second)
	if err != nil {
		t.Fatalf("CanonicalJSONHash(second): %v", err)
	}
	if firstHash != secondHash {
		t.Fatalf("canonical hashes differ: %q vs %q", firstHash, secondHash)
	}
	if got, want := firstHash, sha256Hex(firstCanonical); got != want {
		t.Fatalf("CanonicalJSONHash = %q, want sha256 of canonical JSON %q", got, want)
	}
}

func TestCanonicalJSONArrayOrderIsPreserved(t *testing.T) {
	t.Parallel()

	got, err := tools.CanonicalJSON([]byte(`[3,1,2,1]`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != `[3,1,2,1]` {
		t.Fatalf("array canonical = %s, want [3,1,2,1]", got)
	}
}

func TestCanonicalJSONPreservesJSONNumberLiterals(t *testing.T) {
	t.Parallel()

	got, err := tools.CanonicalJSON(json.RawMessage(`{"exp":1e2,"dec":1.50,"big":9007199254740993}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != `{"big":9007199254740993,"dec":1.50,"exp":1e2}` {
		t.Fatalf("number canonical = %s", got)
	}
}

func TestCanonicalJSONRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: "canonicalize JSON"},
		{name: "truncated", input: `{"a":`, want: "canonicalize JSON"},
		{name: "trailing value", input: `{} {}`, want: "trailing data"},
		{name: "trailing scalar", input: `null true`, want: "trailing data"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tools.CanonicalJSON([]byte(tc.input)); err == nil {
				t.Fatalf("CanonicalJSON(%q) succeeded, want error", tc.input)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CanonicalJSON(%q) error = %v, want %q", tc.input, err, tc.want)
			}
			if _, err := tools.CanonicalJSONHash([]byte(tc.input)); err == nil {
				t.Fatalf("CanonicalJSONHash(%q) succeeded, want error", tc.input)
			}
		})
	}
}
