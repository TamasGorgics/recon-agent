package untrusted

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultMaxRunes is the sanitized body limit when Sanitizer.MaxRunes is not
// positive.
const DefaultMaxRunes = 4096

// TruncationMarker is appended once when the sanitized text exceeds the rune
// limit. It is ASCII so it survives a second sanitize pass unchanged.
const TruncationMarker = " [truncated]"

// Sanitizer cleans and bounds one untrusted value. The zero value uses
// DefaultMaxRunes. MaxRunes is the maximum number of runes in the total output,
// including any truncation marker.
type Sanitizer struct {
	MaxRunes int
}

// Sanitize cleans text with the default rune limit.
func Sanitize(text string) string {
	return Sanitizer{}.Sanitize(text)
}

// Sanitize removes control and format characters, normalizes line endings,
// decodes invalid UTF-8 as U+FFFD, and truncates the result by rune count.
//
// Rules (spec section 3.3):
//   - CRLF and lone CR become LF; LF and TAB are preserved.
//   - Every other Unicode Cc control is removed (C0, C1, NUL, DEL, ESC).
//   - Every Unicode Cf format character is removed, including bidi controls,
//     zero-width characters, BOM, and the Unicode tag-character range.
//   - Invalid UTF-8 bytes are decoded as U+FFFD; output is always valid UTF-8
//     and never splits a multi-byte rune.
//   - The total output is at most MaxRunes runes. When truncation occurs the
//     body keeps MaxRunes-len(TruncationMarker) runes and the marker is
//     appended exactly once.
//   - The empty string sanitizes to the empty string.
//
// Sanitize is idempotent: Sanitize(Sanitize(s)) == Sanitize(s).
func (s Sanitizer) Sanitize(text string) string {
	if text == "" {
		return ""
	}
	max := s.MaxRunes
	if max <= 0 {
		max = DefaultMaxRunes
	}
	minMax := len(TruncationMarker) + 1
	if max < minMax {
		max = minMax
	}

	// Collect at most max+1 runes: one extra rune is enough to know that
	// truncation is required, and bounded cleaning keeps hostile inputs cheap.
	cleaned := make([]rune, 0, min(len(text), max+1))
	for len(text) > 0 {
		r, size := decodeRune(text)
		text = text[size:]
		if r == '\r' {
			// Normalize CRLF and lone CR to LF. The LF byte, if present, is
			// consumed here and never emitted twice.
			if len(text) > 0 && text[0] == '\n' {
				text = text[1:]
			}
			r = '\n'
		} else if r != '\n' && r != '\t' && isForbidden(r) {
			continue
		}
		cleaned = append(cleaned, r)
		if len(cleaned) > max {
			break
		}
	}

	if len(cleaned) <= max {
		return string(cleaned)
	}
	body := max - len(TruncationMarker)
	return string(cleaned[:body]) + TruncationMarker
}

// decodeRune decodes one rune and replaces invalid UTF-8 with U+FFFD, consuming
// exactly one byte for a malformed sequence so it can never swallow a following
// valid rune.
func decodeRune(text string) (rune, int) {
	return utf8.DecodeRuneInString(text)
}

// isForbidden reports whether r must be removed: Cc controls (other than the
// LF/TAB exceptions handled by the caller), Cf format characters, and the
// explicit bidi/zero-width/tag ranges from spec section 3.3.
func isForbidden(r rune) bool {
	if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
		return true
	}
	switch {
	case r >= 0x202A && r <= 0x202E: // bidi embedding/override
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0x200E || r == 0x200F: // LRM/RLM
		return true
	case r >= 0x200B && r <= 0x200D: // zero-width space/non-joiner/joiner
		return true
	case r == 0x2060: // word joiner
		return true
	case r == 0xFEFF: // BOM / zero-width no-break space
		return true
	case r == 0x180E: // Mongolian vowel separator
		return true
	case r >= 0xE0000 && r <= 0xE007F: // Unicode tag characters
		return true
	}
	return false
}

// escapeBoundary replaces every exact occurrence of token with marker. It is
// the only way a value is allowed to mention the boundary token.
func escapeBoundary(value, token, marker string) string {
	if token == "" {
		return value
	}
	return strings.ReplaceAll(value, token, marker)
}
