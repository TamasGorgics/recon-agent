package tools_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

func TestParseUUID(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		testCaseID,
		strings.ToUpper(testCaseID),
	} {
		got, err := tools.ParseUUID(value)
		if err != nil {
			t.Errorf("ParseUUID(%q) = %v", value, err)
			continue
		}
		if got != value {
			t.Errorf("ParseUUID(%q) = %q, want the same value", value, got)
		}
	}

	for _, value := range []string{"", "not-a-uuid", "11111111-1111-1111-1111-11111111111"} {
		if _, err := tools.ParseUUID(value); err == nil {
			t.Errorf("ParseUUID(%q) = nil, want an error", value)
		}
	}
}

func TestParseDateStrictUTC(t *testing.T) {
	t.Parallel()

	got, err := tools.ParseDate("2026-01-02")
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	if got.Format("2006-01-02") != "2026-01-02" || got.Location() != time.UTC {
		t.Fatalf("ParseDate = %v (%v), want 2026-01-02 UTC", got, got.Location())
	}

	for _, value := range []string{"", "2026-1-2", "2026-01-02T00:00:00Z", "2026-02-30", "02-01-2026"} {
		if _, err := tools.ParseDate(value); err == nil {
			t.Errorf("ParseDate(%q) = nil, want an error", value)
		}
	}
}

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	currency, err := tools.ParseCurrency("USD")
	if err != nil {
		t.Fatalf("ParseCurrency(USD): %v", err)
	}
	if currency.Code() != "USD" {
		t.Fatalf("currency = %q, want USD", currency.Code())
	}
	for _, value := range []string{"", "usd", "US", "XXX"} {
		if _, err := tools.ParseCurrency(value); err == nil {
			t.Errorf("ParseCurrency(%q) = nil, want an error", value)
		}
	}
}

func TestFormatDateAndTimestamp(t *testing.T) {
	t.Parallel()

	stockholm, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	value := time.Date(2026, 1, 2, 23, 4, 5, 0, stockholm)

	if got := tools.FormatTimestamp(value); got != "2026-01-02T22:04:05Z" {
		t.Fatalf("FormatTimestamp = %q, want 2026-01-02T22:04:05Z", got)
	}
	if got := tools.FormatDate(value); got != "2026-01-02" {
		t.Fatalf("FormatDate = %q, want 2026-01-02", got)
	}
}

func TestDecodeArgsRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	type request struct {
		CaseID string `json:"case_id"`
	}
	var dst request
	if err := tools.DecodeArgs(json.RawMessage(`{"case_id":"x"}`), &dst); err != nil {
		t.Fatalf("DecodeArgs: %v", err)
	}
	if dst.CaseID != "x" {
		t.Fatalf("CaseID = %q, want x", dst.CaseID)
	}
	if err := tools.DecodeArgs(json.RawMessage(`{"case_id":"x","extra":1}`), &dst); err == nil {
		t.Fatal("DecodeArgs accepted an unknown field")
	}
	if err := tools.DecodeArgs(json.RawMessage(`{"case_id":"x"}{}`), &dst); err == nil {
		t.Fatal("DecodeArgs accepted trailing data")
	}
}

func TestListUntrustedFields(t *testing.T) {
	t.Parallel()

	configured := map[string][]string{"psp_a": {"memo"}}
	tests := []struct {
		name       string
		source     string
		raw        map[string]any
		configured map[string][]string
		want       []string
	}{
		{
			name:       "configured plus reference",
			source:     "psp_a",
			raw:        map[string]any{"memo": "hello", "reference": "r1"},
			configured: configured,
			want:       []string{"memo", "reference"},
		},
		{
			name:       "configured field absent",
			source:     "psp_a",
			raw:        map[string]any{"reference": nil},
			configured: configured,
			want:       []string{"reference"},
		},
		{
			name:       "configured field null",
			source:     "psp_a",
			raw:        map[string]any{"memo": nil},
			configured: configured,
			want:       []string{},
		},
		{
			name:       "bank reference",
			source:     "bank_x",
			raw:        map[string]any{"description": "d", "bank_reference": "b"},
			configured: nil,
			want:       []string{"bank_reference", "description"},
		},
		{
			name:       "fallback defaults",
			source:     "psp_a",
			raw:        map[string]any{"memo": "hello"},
			configured: nil,
			want:       []string{"memo"},
		},
		{
			name:       "unknown source",
			source:     "other",
			raw:        map[string]any{"memo": "hello"},
			configured: nil,
			want:       []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tools.ListUntrustedFields(tt.source, tt.raw, tt.configured)
			if got == nil {
				t.Fatal("ListUntrustedFields returned nil, want an empty slice")
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ListUntrustedFields = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSortedUnique(t *testing.T) {
	t.Parallel()

	got := tools.SortedUnique([]string{"b", "a", "b", "", "a"})
	want := []string{"", "a", "b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("SortedUnique = %v, want %v", got, want)
	}
}

func TestEncodeJSON(t *testing.T) {
	t.Parallel()

	got, err := tools.EncodeJSON(map[string]any{"b": 2, "a": 1})
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if string(got) != `{"a":1,"b":2}` {
		t.Fatalf("EncodeJSON = %s, want deterministic key order", got)
	}
}
