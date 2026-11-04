// migrationlint scans SQL migrations for floating-point column types (FIN-01).
//
// It tokenizes SQL rather than using regular expressions so that REAL,
// DOUBLE PRECISION, and FLOAT are reported only when they appear as unquoted
// words. Line comments, nested block comments, quoted identifiers, and string
// literals (including dollar-quoted strings) are ignored.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding is one forbidden floating-point type in a migration file.
type Finding struct {
	Path   string
	Line   int
	Column int
	Rule   string // "REAL", "DOUBLE PRECISION", or "FLOAT"
}

// String renders the finding in compiler-style path:line:column form with the
// FIN-01 rule and the required replacement.
func (f Finding) String() string {
	return fmt.Sprintf(
		"%s:%d:%d: %s is not allowed in migrations (FIN-01); use BIGINT minor units or NUMERIC for exact decimals",
		f.Path, f.Line, f.Column, f.Rule,
	)
}

// ScanError is an operational failure (missing directory, unreadable file).
// It is distinct from a Finding so callers can tell a lint failure (exit 1)
// from an infrastructure failure (exit 2).
type ScanError struct {
	Path string
	Err  error
}

func (e *ScanError) Error() string {
	return fmt.Sprintf("%s: %v", e.Path, e.Err)
}

func (e *ScanError) Unwrap() error { return e.Err }

// CheckDir scans every *.sql file directly inside dir and returns findings in
// filename then source order.
func CheckDir(dir string) ([]Finding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, &ScanError{Path: dir, Err: err}
	}

	var findings []Finding
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".sql") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, &ScanError{Path: path, Err: err}
		}
		findings = append(findings, CheckFile(path, src)...)
	}
	return findings, nil
}

// CheckFile scans SQL source and reports floating-point type words. Position
// information is 1-based and refers to the first byte of the reported word.
func CheckFile(path string, src []byte) []Finding {
	lineStarts := []int{0}
	for i, b := range src {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	tokens := tokenize(src)
	var findings []Finding
	for i := 0; i < len(tokens); i++ {
		upper := strings.ToUpper(tokens[i].text)
		switch upper {
		case "REAL", "FLOAT":
			line, column := position(lineStarts, tokens[i].offset)
			findings = append(findings, Finding{Path: path, Line: line, Column: column, Rule: upper})
		case "DOUBLE":
			if i+1 < len(tokens) && strings.EqualFold(tokens[i+1].text, "PRECISION") {
				line, column := position(lineStarts, tokens[i].offset)
				findings = append(findings, Finding{Path: path, Line: line, Column: column, Rule: "DOUBLE PRECISION"})
				i++
			}
		}
	}
	return findings
}

func position(lineStarts []int, offset int) (line, column int) {
	index := sort.Search(len(lineStarts), func(i int) bool { return lineStarts[i] > offset }) - 1
	if index < 0 {
		index = 0
	}
	return index + 1, offset - lineStarts[index] + 1
}

type sqlToken struct {
	text   string
	offset int
}

// tokenize returns unquoted SQL words and their byte offsets. Comments,
// quoted identifiers, and string literals are skipped entirely.
func tokenize(src []byte) []sqlToken {
	var tokens []sqlToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case isSQLSpace(c):
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i = skipBlockComment(src, i)
		case c == '\'' || c == '"':
			i = skipQuoted(src, i, c)
		case c == '$':
			if end, ok := skipDollarQuoted(src, i); ok {
				i = end
			} else {
				i++
			}
		case isIdentStart(c):
			start := i
			i++
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			tokens = append(tokens, sqlToken{text: string(src[start:i]), offset: start})
		default:
			i++
		}
	}
	return tokens
}

func skipBlockComment(src []byte, i int) int {
	depth := 1
	i += 2
	for i < len(src) && depth > 0 {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			depth++
			i += 2
		case src[i] == '*' && i+1 < len(src) && src[i+1] == '/':
			depth--
			i += 2
		default:
			i++
		}
	}
	return i
}

func skipQuoted(src []byte, i int, quote byte) int {
	// E'...' strings allow backslash escapes; standard strings do not.
	escape := quote == '\'' && i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') &&
		(i == 1 || !isIdentPart(src[i-2]))
	i++
	for i < len(src) {
		if escape && src[i] == '\\' && i+1 < len(src) {
			i += 2
			continue
		}
		if src[i] == quote {
			if i+1 < len(src) && src[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

// skipDollarQuoted recognizes PostgreSQL dollar-quoted strings ($$...$$ and
// $tag$...$tag$) starting at the opening dollar sign. It returns the offset
// after the closing delimiter.
func skipDollarQuoted(src []byte, i int) (end int, ok bool) {
	j := i + 1
	var tag []byte
	switch {
	case j < len(src) && src[j] == '$':
		// Empty tag.
	case j < len(src) && isTagStart(src[j]):
		start := j
		j++
		for j < len(src) && isTagPart(src[j]) {
			j++
		}
		if j >= len(src) || src[j] != '$' {
			return 0, false
		}
		tag = src[start:j]
	default:
		return 0, false
	}

	openEnd := j + 1
	closing := append(append([]byte{'$'}, tag...), '$')
	rel := bytes.Index(src[openEnd:], closing)
	if rel < 0 {
		return len(src), true
	}
	return openEnd + rel + len(closing), true
}

func isSQLSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

func isTagStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isTagPart(c byte) bool {
	return isTagStart(c) || (c >= '0' && c <= '9')
}
