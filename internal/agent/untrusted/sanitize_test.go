package untrusted_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
)

func TestSanitizeCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"crlf and lone cr", "a\r\nb\rc", "a\nb\nc"},
		{"newline and tab preserved", "a\nb\tc", "a\nb\tc"},
		{"nul and c0 controls", "\x00a\x01b\x1bc\x7fd", "abcd"},
		{"c1 controls", "\u0085a\u009fb", "ab"},
		{"bidi embeddings and overrides", "a\u202ab\u202bc\u202cd\u202de\u202ef", "abcdef"},
		{"bidi isolates", "a\u2066b\u2067c\u2068d\u2069e", "abcde"},
		{"bidi marks", "a\u200eb\u200fc", "abc"},
		{"zero width", "a\u200bb\u200cc\u200dd\u2060e\ufefff\u180eg", "abcdefg"},
		{"unicode tags", "a\U000E0001b\U000E0041c\U000E007Fd", "abcd"},
		{"invalid utf8", "\xffabc", "\uFFFDabc"},
		{"valid replacement rune preserved", "a\uFFFDb", "a\uFFFDb"},
		{"multibyte and emoji", "héllo 🚀", "héllo 🚀"},
		{"terminal escape sequence", "safe\x1b[31mred\x1b[0m", "safe[31mred[0m"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := untrusted.Sanitize(tc.in)
			if got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Sanitize(%q) returned invalid UTF-8 %q", tc.in, got)
			}
			assertNoForbiddenRunes(t, got)
			if second := untrusted.Sanitize(got); second != got {
				t.Fatalf("Sanitize is not idempotent: %q -> %q", got, second)
			}
		})
	}
}

func TestSanitizeTruncatesAtRuneBoundary(t *testing.T) {
	t.Parallel()

	// 30 runes: ASCII then multi-byte and emoji. Max 20 includes the 12-rune
	// marker, so the body is exactly 8 runes.
	input := "abcdefgh" + "é" + "🚀" + strings.Repeat("x", 20)
	got := untrusted.Sanitizer{MaxRunes: 22}.Sanitize(input)
	runes := []rune(got)
	if len(runes) != 22 {
		t.Fatalf("truncated rune count = %d, want 22 (%q)", len(runes), got)
	}
	if !strings.HasSuffix(got, untrusted.TruncationMarker) {
		t.Fatalf("truncated output does not end with marker: %q", got)
	}
	if strings.Count(got, untrusted.TruncationMarker) != 1 {
		t.Fatalf("truncation marker appears %d times, want 1", strings.Count(got, untrusted.TruncationMarker))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "abcdefghé🚀") {
		t.Fatalf("truncation split the rune boundary: %q", got)
	}
	if second := (untrusted.Sanitizer{MaxRunes: 22}).Sanitize(got); second != got {
		t.Fatalf("truncation is not idempotent: %q -> %q", got, second)
	}
}

func TestSanitizeTruncationEdgeCases(t *testing.T) {
	t.Parallel()

	markerRunes := utf8.RuneCountInString(untrusted.TruncationMarker)

	t.Run("exactly max is unchanged", func(t *testing.T) {
		body := strings.Repeat("a", 20)
		if got := (untrusted.Sanitizer{MaxRunes: 20}).Sanitize(body); got != body {
			t.Fatalf("exactly-max input changed: %q", got)
		}
	})

	t.Run("max below marker minimum is clamped", func(t *testing.T) {
		got := (untrusted.Sanitizer{MaxRunes: 1}).Sanitize(strings.Repeat("a", 100))
		if count := utf8.RuneCountInString(got); count != markerRunes+1 {
			t.Fatalf("clamped rune count = %d, want %d (%q)", count, markerRunes+1, got)
		}
		if !strings.HasSuffix(got, untrusted.TruncationMarker) {
			t.Fatalf("clamped output does not end with marker: %q", got)
		}
		if strings.Count(got, untrusted.TruncationMarker) != 1 {
			t.Fatalf("clamped marker count = %d, want 1", strings.Count(got, untrusted.TruncationMarker))
		}
	})

	t.Run("default max applies", func(t *testing.T) {
		long := strings.Repeat("é", untrusted.DefaultMaxRunes+10)
		got := untrusted.Sanitize(long)
		if count := utf8.RuneCountInString(got); count != untrusted.DefaultMaxRunes {
			t.Fatalf("default truncated rune count = %d, want %d", count, untrusted.DefaultMaxRunes)
		}
	})
}

func TestSanitizeNoForbiddenProperty(t *testing.T) {
	t.Parallel()

	messy := "\x00\r\n\t\u202e\u2066\u200b\u200d\u2060\ufeff\U000E0041abc\x1b"
	got := untrusted.Sanitize(messy)
	assertNoForbiddenRunes(t, got)
	if got != "\n\t"+"abc" {
		t.Fatalf("Sanitize(messy) = %q, want newline/tab then abc", got)
	}
}

// assertNoForbiddenRunes asserts the sanitizer output property: valid UTF-8,
// no Cc other than LF/TAB, and no Cf.
func assertNoForbiddenRunes(t *testing.T, text string) {
	t.Helper()
	if !utf8.ValidString(text) {
		t.Fatalf("output is not valid UTF-8: %q", text)
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.Is(unicode.Cc, r) {
			t.Fatalf("output retains control rune U+%04X: %q", r, text)
		}
		if unicode.Is(unicode.Cf, r) {
			t.Fatalf("output retains format rune U+%04X: %q", r, text)
		}
		switch {
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069,
			r == 0x200E, r == 0x200F, r >= 0x200B && r <= 0x200D,
			r == 0x2060, r == 0xFEFF, r == 0x180E,
			r >= 0xE0000 && r <= 0xE007F:
			t.Fatalf("output retains explicitly forbidden rune U+%04X: %q", r, text)
		}
	}
}
