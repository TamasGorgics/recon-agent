package untrusted_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/agent/untrusted"
	"github.com/TamasGorgics/recon-agent/synth/injections"
)

// FuzzSanitize checks the four sanitizer properties on every input: valid
// UTF-8, no Cc other than LF/TAB and no Cf, bounded total rune count, and
// idempotence.
func FuzzSanitize(f *testing.F) {
	templates, err := injections.All()
	if err != nil {
		f.Fatalf("load injection templates: %v", err)
	}
	for _, template := range templates {
		f.Add(template.Text)
	}
	f.Add("\x00\x01\x1b\x7f\u0085\u009f")
	f.Add("\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f")
	f.Add("\u200b\u200c\u200d\u2060\ufeff\u180e\U000E0001\U000E007f")
	f.Add("\xff\xfe\xc0\x80")
	f.Add(strings.Repeat("é🚀", 5000))
	f.Add("")

	f.Fuzz(func(t *testing.T, text string) {
		got := untrusted.Sanitize(text)
		if !utf8.ValidString(got) {
			t.Fatalf("output is not valid UTF-8: %q", got)
		}
		if count := utf8.RuneCountInString(got); count > untrusted.DefaultMaxRunes {
			t.Fatalf("output rune count = %d, max %d", count, untrusted.DefaultMaxRunes)
		}
		for _, r := range got {
			if r == '\n' || r == '\t' {
				continue
			}
			if unicode.Is(unicode.Cc, r) {
				t.Fatalf("output retains control rune U+%04X", r)
			}
			if unicode.Is(unicode.Cf, r) {
				t.Fatalf("output retains format rune U+%04X", r)
			}
			if r >= 0xE0000 && r <= 0xE007F {
				t.Fatalf("output retains tag rune U+%04X", r)
			}
		}
		if second := untrusted.Sanitize(got); second != got {
			t.Fatalf("Sanitize is not idempotent: %q -> %q", got, second)
		}
	})
}
