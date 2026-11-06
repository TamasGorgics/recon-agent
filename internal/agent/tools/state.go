package tools

import (
	"sort"
	"strings"
	"sync"
)

// Escalation is the terminal signal a flagged or blocked run hands to the
// loop. It is a value copy: mutating the result of Escalation cannot change the
// run state.
type Escalation struct {
	Reason  string
	Summary string
}

// RunState is the per-run state shared by every tool call. M2-07 owns it; M2-08
// consumes its methods and does not add fields to RunContext. All methods are
// safe for concurrent use.
//
// A run with at least one injection flag may only end with escalate. Proposal
// handlers consult Flagged (or ProposalGuard); the loop in M2-09 also forces
// the run outcome to escalated.
type RunState struct {
	mu          sync.Mutex
	flags       []string
	proposalKey string
	escalation  *Escalation
}

func newRunState() *RunState {
	return &RunState{flags: []string{}}
}

// AddFlags records one or more stable injection-flag codes. Empty strings are
// ignored, duplicates are collapsed, and the stored list stays sorted.
func (s *RunState) AddFlags(codes ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flags = normalizeFlagCodes(append(s.flags, codes...))
}

// Flags returns a sorted, unique copy of the run's injection-flag codes. The
// result is never nil.
func (s *RunState) Flags() []string {
	if s == nil {
		return []string{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.flags...)
}

// Flagged reports whether the run has at least one injection flag.
func (s *RunState) Flagged() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flags) > 0
}

// ProposalGuard returns an injection_flagged CodedError when the run is
// flagged. Proposal handlers call it at the start of validation; escalate is
// exempt.
func (s *RunState) ProposalGuard() error {
	if !s.Flagged() {
		return nil
	}
	return &CodedError{
		Code:    CodeInjectionFlagged,
		Message: "run has injection flags; only escalate is allowed",
	}
}

// ProposalKey returns the idempotency key of the proposal this run has already
// consumed, or the empty string when the slot is unused.
func (s *RunState) ProposalKey() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proposalKey
}

// MarkProposalUsed consumes the run's one-proposal slot. It returns true when
// key was already set or key matches the existing key; it returns false and
// changes nothing for an empty key or a different key.
func (s *RunState) MarkProposalUsed(key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proposalKey == "" {
		s.proposalKey = key
		return true
	}
	return s.proposalKey == key
}

// SetEscalation records the terminal escalation signal. A reason that is empty
// after trimming is ignored. The first non-empty escalation wins; later calls
// cannot replace a terminal signal.
func (s *RunState) SetEscalation(reason, summary string) {
	if s == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.escalation != nil {
		return
	}
	s.escalation = &Escalation{Reason: reason, Summary: summary}
}

// Escalation returns a copy of the terminal escalation signal, or nil when
// unset.
func (s *RunState) Escalation() *Escalation {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.escalation == nil {
		return nil
	}
	copy := *s.escalation
	return &copy
}

// normalizeFlagCodes trims, drops empties, de-duplicates and sorts flag codes.
// The result is never nil.
func normalizeFlagCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	normalized := make([]string, 0, len(codes))
	for _, code := range codes {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		if _, duplicate := seen[code]; duplicate {
			continue
		}
		seen[code] = struct{}{}
		normalized = append(normalized, code)
	}
	sort.Strings(normalized)
	return normalized
}
