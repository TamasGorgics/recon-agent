package api

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 11, 10, 0, 0, 123456789, time.UTC)
	encoded := encodeCursor(created, testCaseID)
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !decoded.CreatedAt.Equal(created) {
		t.Fatalf("created_at = %v, want %v", decoded.CreatedAt, created)
	}
	if decoded.ID != testCaseID {
		t.Fatalf("id = %q", decoded.ID)
	}
}

func TestCursorRejectsInvalidValues(t *testing.T) {
	valid := encodeCursor(time.Date(2026, 11, 11, 10, 0, 0, 123456789, time.UTC), testCaseID)
	tests := []string{
		"not-base64",
		"",
		"e30",                             // {}
		"eyJ0IjoiMjAyNiIsImlkIjoiYToxIn0", // malformed id
	}
	for _, value := range tests {
		if _, err := decodeCursor(value); err == nil {
			t.Fatalf("decodeCursor(%q) succeeded", value)
		}
	}
	if _, err := decodeCursor(strings.TrimSuffix(valid, "=")); err != nil {
		t.Fatalf("valid cursor failed: %v", err)
	}
}

func TestCursorRejectsDuplicateKeysAndTrailingBytes(t *testing.T) {
	payload := `{"t":"2026-11-11T10:00:00.123456789Z","id":"` + testCaseID + `"}`
	duplicateTime := `{"t":"2026-11-11T10:00:00.123456789Z","t":"2026-11-11T11:00:00Z","id":"` + testCaseID + `"}`
	duplicateID := `{"t":"2026-11-11T10:00:00.123456789Z","id":"` + testCaseID + `","id":"` + testCaseID + `"}`
	tests := map[string]string{
		"duplicate time key": duplicateTime,
		"duplicate id key":   duplicateID,
		"trailing object":    payload + `{}`,
		"trailing garbage":   payload + "x",
		"two documents":      payload + ` ` + payload,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))
			if _, err := decodeCursor(encoded); err == nil {
				t.Fatalf("decodeCursor accepted %s", raw)
			}
		})
	}
}

func TestParseListLimit(t *testing.T) {
	tests := []struct {
		values []string
		want   int
		ok     bool
	}{
		{nil, 50, true},
		{[]string{"1"}, 1, true},
		{[]string{"200"}, 200, true},
		{[]string{"0"}, 0, false},
		{[]string{"201"}, 0, false},
		{[]string{"abc"}, 0, false},
		{[]string{""}, 0, false},
		{[]string{"1", "2"}, 0, false},
	}
	for _, test := range tests {
		got, err := parseListLimit(test.values)
		if (err == nil) != test.ok || got != test.want {
			t.Fatalf("parseListLimit(%v) = %d, %v; want %d, ok=%v", test.values, got, err, test.want, test.ok)
		}
	}
}

func TestParseISODate(t *testing.T) {
	valid := map[string]time.Time{
		"2026-11-01": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"2024-02-29": time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
	}
	for input, want := range valid {
		got, ok := parseISODate(input)
		if !ok || !got.Equal(want) {
			t.Fatalf("parseISODate(%q) = %v, %v; want %v", input, got, ok, want)
		}
	}
	for _, input := range []string{"2026-1-1", "2026-11-1", "2026/11/01", "2026-13-01", "2026-02-30", "2026-11-01T00:00:00Z", "0000-00-00"} {
		if _, ok := parseISODate(input); ok {
			t.Fatalf("parseISODate(%q) accepted an invalid date", input)
		}
	}
}

func TestSanitizeRationale(t *testing.T) {
	if got, want := sanitizeRationale("hello\x00\x01world"), "helloworld"; got != want {
		t.Fatalf("sanitizeRationale = %q, want %q", got, want)
	}
	long := strings.Repeat("é", maxRationaleRunes+10)
	got := sanitizeRationale(long)
	if len([]rune(got)) != maxRationaleRunes {
		t.Fatalf("truncated rationale has %d runes, want %d", len([]rune(got)), maxRationaleRunes)
	}
}
