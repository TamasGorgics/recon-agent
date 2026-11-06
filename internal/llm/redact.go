package llm

import (
	"regexp"
	"strings"
)

// Redaction classes recorded in cassette metadata.
const (
	RedactionAPIKey        = "api_key"
	RedactionBearer        = "bearer"
	RedactionAuthorization = "authorization"
	RedactionSessionID     = "session_id"
)

var (
	apiKeyPattern              = regexp.MustCompile(`oc_sk_[A-Za-z0-9_-]+`)
	bearerPattern              = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/_=-]+`)
	bearerQuotedPattern        = regexp.MustCompile(`(?i)(\bBearer\s+)"(?:[^"\\]|\\.)*"`)
	bearerEscapedQuotedPattern = regexp.MustCompile(`(?i)(\bBearer\s+)\\"(?:[^"\\]|\\.)*\\"`)
	authorizationPrefixPattern = regexp.MustCompile(`(?i)\bauthorization["']?\s*[:=]\s*`)
	sessionHeaderPrefixPattern = regexp.MustCompile(`(?i)\bx-opencode-session["']?\s*[:=]\s*`)
	sessionIDPattern           = regexp.MustCompile(`ses_[A-Za-z0-9_-]+`)
)

const redacted = "[REDACTED]"

// Redact removes API keys, bearer values, authorization headers, and OpenCode
// session identifiers from s. Header values are replaced without consuming JSON
// punctuation, so a valid JSON document stays valid. It is idempotent.
func Redact(s string) string {
	out, _ := RedactWithClasses(s)
	return out
}

// RedactBytes removes the same secret classes from b.
func RedactBytes(b []byte) []byte {
	return []byte(Redact(string(b)))
}

// RedactWithClasses returns the redacted text and the redaction classes that
// were applied, in stable order.
func RedactWithClasses(s string) (string, []string) {
	classes := make([]string, 0, 4)
	if apiKeyPattern.MatchString(s) {
		classes = append(classes, RedactionAPIKey)
	}
	if bearerPattern.MatchString(s) || bearerQuotedPattern.MatchString(s) || bearerEscapedQuotedPattern.MatchString(s) {
		classes = append(classes, RedactionBearer)
	}
	if authorizationPrefixPattern.MatchString(s) {
		classes = append(classes, RedactionAuthorization)
	}
	if sessionHeaderPrefixPattern.MatchString(s) || sessionIDPattern.MatchString(s) {
		classes = append(classes, RedactionSessionID)
	}
	s = apiKeyPattern.ReplaceAllString(s, redacted)
	s = bearerEscapedQuotedPattern.ReplaceAllString(s, `${1}\"`+redacted+`\"`)
	s = bearerQuotedPattern.ReplaceAllString(s, `${1}"`+redacted+`"`)
	s = bearerPattern.ReplaceAllString(s, redacted)
	s, _ = redactHeaderValues(s, authorizationPrefixPattern)
	s, _ = redactHeaderValues(s, sessionHeaderPrefixPattern)
	s = sessionIDPattern.ReplaceAllString(s, redacted)
	return s, classes
}

// redactHeaderValues replaces only the value that follows each header-like
// prefix. The prefix and any JSON delimiters around the value are preserved.
func redactHeaderValues(s string, prefix *regexp.Regexp) (string, bool) {
	matches := prefix.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s, false
	}
	var out strings.Builder
	last := 0
	changed := false
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		valueStart, valueEnd := headerValueBounds(s, match[1])
		if valueStart >= valueEnd {
			continue
		}
		if s[valueStart:valueEnd] == redacted {
			continue
		}
		out.WriteString(s[last:valueStart])
		out.WriteString(redacted)
		last = valueEnd
		changed = true
	}
	if !changed {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// headerValueBounds returns the half-open range of the value following start.
// Quoted values are replaced inside their quotes; unquoted values stop at JSON
// punctuation, whitespace, or a newline. The surrounding bytes are never part
// of the range, so punctuation is preserved.
func headerValueBounds(s string, start int) (int, int) {
	if start >= len(s) {
		return start, start
	}
	if start+1 < len(s) && s[start] == '\\' && (s[start+1] == '"' || s[start+1] == '\'') {
		quote := s[start+1]
		valueStart := start + 2
		for i := valueStart; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) && s[i+1] == quote {
				return valueStart, i
			}
			if s[i] == quote {
				return valueStart, i
			}
		}
		lineEnd := strings.IndexAny(s[valueStart:], "\r\n")
		if lineEnd < 0 {
			return valueStart, len(s)
		}
		return valueStart, valueStart + lineEnd
	}
	if quote := s[start]; quote == '"' || quote == '\'' {
		valueStart := start + 1
		for i := valueStart; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == quote {
				return valueStart, i
			}
		}
		lineEnd := strings.IndexAny(s[valueStart:], "\r\n")
		if lineEnd < 0 {
			return valueStart, len(s)
		}
		return valueStart, valueStart + lineEnd
	}
	if strings.HasPrefix(s[start:], redacted) {
		return start, start + len(redacted)
	}
	end := start
	for end < len(s) {
		switch s[end] {
		case ' ', '\t', '\r', '\n', '"', '\'', ',', '}', ']', '{', '[':
			return start, end
		}
		end++
	}
	return start, end
}
