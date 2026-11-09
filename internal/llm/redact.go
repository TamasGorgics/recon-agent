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
	// Header prefixes may be plain text, JSON keys, or JSON keys nested inside
	// one or two levels of escaped strings. For example all three forms below
	// share the same prefix:
	//
	//	authorization: ...
	//	"authorization": ...
	//	\"authorization\": ...       (one escaped level)
	//	\\\"authorization\\\": ...   (two escaped levels)
	//
	// The optional backslash runs consume the escaped quotes around the key.
	authorizationPrefixPattern = regexp.MustCompile(
		`(?i)\b(?:proxy[-_]?)?authorization\\{0,3}["']?\s*\\{0,3}["']?\s*[:=]\s*`)
	sessionHeaderPrefixPattern = regexp.MustCompile(
		`(?i)\bx-opencode-session\\{0,3}["']?\s*\\{0,3}["']?\s*[:=]\s*`)
	sessionIDPattern = regexp.MustCompile(`ses_[A-Za-z0-9_-]+`)
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
	// Header values must be redacted before the bare bearer pattern: otherwise
	// "Authorization: Bearer first second" loses only its first token and the
	// tail survives. The header redactor consumes the whole value.
	s, _ = redactHeaderValues(s, authorizationPrefixPattern)
	s, _ = redactHeaderValues(s, sessionHeaderPrefixPattern)
	s = bearerPattern.ReplaceAllString(s, redacted)
	s = sessionIDPattern.ReplaceAllString(s, redacted)
	return s, classes
}

// headerValueSpan is one replacement: the half-open source range and the text
// that replaces it. Quoted spans include their opening and closing quote bytes
// so the replacement can keep the same escaping level as the input.
type headerValueSpan struct {
	start       int
	end         int
	replacement string
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
		span, ok := headerValueSpanFor(s, match[1])
		if !ok {
			continue
		}
		out.WriteString(s[last:span.start])
		out.WriteString(span.replacement)
		last = span.end
		changed = true
	}
	if !changed {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// headerValueSpanFor returns the span of the header value following start.
//
// Values may be plain ("token"), quoted ("Basic x"), or quoted inside escaped
// JSON (\"Basic x\", \\\"Basic x\\\"). For quoted values the span includes the
// quote bytes and keeps them in the replacement, which keeps a JSON document
// valid at any escaping depth. A header value also owns same-line trailing
// text after its closing quote ("Authorization: \"Basic x\" tail"): that tail
// is part of the value and is redacted with it. Structural JSON bytes and line
// breaks are never consumed, so following lines and JSON delimiters survive.
func headerValueSpanFor(s string, start int) (headerValueSpan, bool) {
	if start >= len(s) || strings.HasPrefix(s[start:], redacted) {
		return headerValueSpan{}, false
	}
	if depth, quote, ok := escapedQuoteAt(s, start); ok {
		return quotedHeaderValueSpan(s, start, start+depth, quote, depth)
	}
	if quote := s[start]; quote == '"' || quote == '\'' {
		return quotedHeaderValueSpan(s, start, start, quote, 0)
	}
	return unquotedHeaderValueSpan(s, start)
}

// escapedQuoteAt reports a quote at start+n preceded by n backslashes. Those
// backslashes are the JSON string escapes around the quote; n is the escaping
// depth (one for \"...\", three bytes for \\\"...\").
func escapedQuoteAt(s string, start int) (int, byte, bool) {
	backslashes := 0
	for start+backslashes < len(s) && s[start+backslashes] == '\\' {
		backslashes++
	}
	if backslashes == 0 || start+backslashes >= len(s) {
		return 0, 0, false
	}
	quote := s[start+backslashes]
	if quote != '"' && quote != '\'' {
		return 0, 0, false
	}
	return backslashes, quote, true
}

// quotedHeaderValueSpan replaces a quoted header value. quoteIndex is the
// opening quote, which is preceded by depth backslashes (zero for plain JSON).
func quotedHeaderValueSpan(s string, spanStart, quoteIndex int, quote byte, depth int) (headerValueSpan, bool) {
	opening := s[spanStart : quoteIndex+1]
	innerStart := quoteIndex + 1
	innerEnd, closingEnd, found := closingQuoteIndex(s, innerStart, quote, depth)
	if !found {
		// Unterminated quoted value: redact the rest of the line, never the
		// line break itself.
		innerEnd = lineEndIndex(s, innerStart)
		closingEnd = innerEnd
	}
	inner := s[innerStart:innerEnd]
	if inner == redacted {
		return headerValueSpan{}, false
	}
	closing := s[innerEnd:closingEnd]
	if found {
		closingEnd = quotedValueTailEnd(s, closingEnd)
	}
	return headerValueSpan{
		start:       spanStart,
		end:         closingEnd,
		replacement: opening + redacted + closing,
	}, true
}

// closingQuoteIndex finds the closing quote for a value whose opening quote is
// escaped at the given depth. A plain quote (depth zero) must not be preceded
// by a backslash; an escaped quote must be preceded by exactly depth
// backslashes. A line break terminates the search.
func closingQuoteIndex(s string, start int, quote byte, depth int) (innerEnd, closingEnd int, found bool) {
	for index := start; index < len(s); {
		if s[index] == '\r' || s[index] == '\n' {
			return index, index, false
		}
		if s[index] == '\\' {
			runEnd := index
			for runEnd < len(s) && s[runEnd] == '\\' {
				runEnd++
			}
			runLength := runEnd - index
			if runEnd < len(s) && s[runEnd] == quote &&
				(depth > 0 && runLength == depth) {
				return index, runEnd + 1, true
			}
			index = runEnd
			continue
		}
		if s[index] == quote {
			// Either a plain closing quote, or the enclosing string's quote
			// when a deeper escaped closing quote is absent. Preserving it
			// keeps the surrounding document balanced.
			return index, index + 1, true
		}
		index++
	}
	return len(s), len(s), false
}

// quotedValueTailEnd extends a redaction span over same-line text that follows
// a closing quote, such as the tail in `authorization: "Basic x" tail`. Text
// that is only structural JSON or whitespace is left untouched so a JSON
// document keeps its shape.
func quotedValueTailEnd(s string, end int) int {
	index := end
	for index < len(s) && (s[index] == ' ' || s[index] == '\t') {
		index++
	}
	if index >= len(s) || structuralHeaderDelimiter(s[index]) {
		return end
	}
	for index < len(s) {
		if s[index] == '\\' && index+1 < len(s) {
			index += 2
			continue
		}
		if s[index] == '\r' || s[index] == '\n' || structuralHeaderDelimiter(s[index]) {
			return index
		}
		index++
	}
	return index
}

// structuralHeaderDelimiter reports bytes that end a header value without being
// part of it. Quotes end values so an enclosing JSON string stays balanced.
func structuralHeaderDelimiter(value byte) bool {
	switch value {
	case '\r', '\n', ',', '}', ']', '{', '[', '"', '\'':
		return true
	default:
		return false
	}
}

// unquotedHeaderValueSpan replaces an unquoted value through the end of its
// line or its enclosing JSON string. Commas and braces inside a JSON string
// are value bytes, not structural delimiters, so they are redacted too; the
// enclosing quote itself is never consumed.
func unquotedHeaderValueSpan(s string, start int) (headerValueSpan, bool) {
	end := start
	for end < len(s) {
		// An escape pair belongs to the value, so an escaped quote inside a
		// JSON string must not terminate it. This keeps the document valid
		// after the quoted-bearer rewrite leaves "Bearer \"...\"" behind.
		if s[end] == '\\' && end+1 < len(s) {
			end += 2
			continue
		}
		if s[end] == '\r' || s[end] == '\n' || s[end] == '"' || s[end] == '\'' {
			break
		}
		end++
	}
	if end == start || s[start:end] == redacted {
		return headerValueSpan{}, false
	}
	return headerValueSpan{start: start, end: end, replacement: redacted}, true
}

func lineEndIndex(s string, start int) int {
	end := start
	for end < len(s) && s[end] != '\r' && s[end] != '\n' {
		end++
	}
	return end
}
