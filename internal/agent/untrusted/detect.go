package untrusted

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Flag is one suspicious-content finding. Code is a stable rule family (for
// example "instruction_override"). Pattern is the configured rule name, never
// matched text. Offset is the byte offset of the first match in the sanitized
// input. No raw value or matched text is ever returned.
type Flag struct {
	Code    string
	Pattern string
	Offset  int
}

// Pattern is one compiled detection rule. Name identifies the rule, Code names
// its stable family, and RE is the compiled RE2 expression.
type Pattern struct {
	Name string
	Code string
	RE   *regexp.Regexp
}

// Detector runs a fixed, immutable list of compiled patterns. A Detector is
// safe for concurrent use.
type Detector struct {
	patterns []Pattern
}

// NewDetector validates a replacement pattern list for tests and future
// configuration. Invalid patterns (nil expression, empty name or code) return
// an error at construction; Suspicious never panics.
func NewDetector(patterns []Pattern) (*Detector, error) {
	compiled := make([]Pattern, 0, len(patterns))
	for i, pattern := range patterns {
		if pattern.Name == "" {
			return nil, fmt.Errorf("untrusted: pattern %d has an empty name", i)
		}
		if pattern.Code == "" {
			return nil, fmt.Errorf("untrusted: pattern %q has an empty code", pattern.Name)
		}
		if pattern.RE == nil {
			return nil, fmt.Errorf("untrusted: pattern %q has no compiled expression", pattern.Name)
		}
		compiled = append(compiled, pattern)
	}
	return &Detector{patterns: compiled}, nil
}

// Suspicious returns at most one Flag per (Code, Pattern) at the first match
// offset, sorted by offset then code then pattern name. It never returns
// matched text. A nil detector returns no flags.
func (d *Detector) Suspicious(text string) []Flag {
	if d == nil || text == "" {
		return []Flag{}
	}
	best := make(map[[2]string]Flag, len(d.patterns))
	for _, pattern := range d.patterns {
		loc := pattern.RE.FindStringIndex(text)
		if loc == nil {
			continue
		}
		key := [2]string{pattern.Code, pattern.Name}
		candidate := Flag{Code: pattern.Code, Pattern: pattern.Name, Offset: loc[0]}
		if existing, ok := best[key]; !ok || candidate.Offset < existing.Offset {
			best[key] = candidate
		}
	}
	flags := make([]Flag, 0, len(best))
	for _, flag := range best {
		flags = append(flags, flag)
	}
	sort.Slice(flags, func(i, j int) bool {
		if flags[i].Offset != flags[j].Offset {
			return flags[i].Offset < flags[j].Offset
		}
		if flags[i].Code != flags[j].Code {
			return flags[i].Code < flags[j].Code
		}
		return flags[i].Pattern < flags[j].Pattern
	})
	return flags
}

var (
	defaultDetectorOnce sync.Once
	defaultDetector     *Detector
	defaultDetectorErr  error
)

// Suspicious runs the default detector over text. Detection is expected to run
// on Sanitize(text); the function does not sanitize for callers.
func Suspicious(text string) []Flag {
	defaultDetectorOnce.Do(func() {
		defaultDetector, defaultDetectorErr = NewDetector(DefaultPatterns())
	})
	if defaultDetectorErr != nil {
		// DefaultPatterns is compiled from Go string literals. A failure here
		// is a programming error; fail safe with no flags and no panic.
		return []Flag{}
	}
	return defaultDetector.Suspicious(text)
}
