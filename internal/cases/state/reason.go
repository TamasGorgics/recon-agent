package state

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Canonical escalation reasons carried by the lifecycle services. Escalation
// reasons are validated free text (run decision for M4-10: the spec 4.4 closed
// set would reject the agent loop's existing model/tool-derived reasons such as
// injection_flagged); these constants keep the lifecycle call sites from
// drifting and are always valid.
const (
	// EscalationAttemptsExhausted is the shared agent/proposal attempt cap being
	// reached.
	EscalationAttemptsExhausted = "attempts_exhausted"
	// EscalationMissingSettlementSLA is the missing-settlement business-day SLA.
	EscalationMissingSettlementSLA = "missing_settlement_sla"
	// EscalationManual is an operator escalation.
	EscalationManual = "manual"
)

// ReasonPartialLateMatch is the only system reason for escalated -> open. A
// partial late match reopens the case for the remaining breaks.
const ReasonPartialLateMatch = "partial_late_match"

// ResolutionSupersededByMatch is reserved for system late-match resolution. A
// human actor may never supply it.
const ResolutionSupersededByMatch = "superseded_by_match"

// maxReasonBytes bounds free-text reasons (spec 4.4). Reasons are rejected, not
// truncated, so a caller cannot silently lose context.
const maxReasonBytes = 500

// maxResolutionBytes bounds a resolution type. The resolution regex is at most
// 64 bytes, so the explicit bound makes the contract readable and keeps a
// future regex change from silently widening the column shape.
const maxResolutionBytes = 64

var resolutionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// reasonRule is the reason requirement for one actor class on one edge.
type reasonRule int

const (
	// reasonOptional accepts an empty reason; a present reason must still pass
	// the free-text validator.
	reasonOptional reasonRule = iota
	// reasonRequiredFreeText requires a 1..500 byte trimmed free-text reason.
	// Escalation targets use this rule too: escalation reasons are validated
	// free text, with the canonical lifecycle values exported as constants.
	reasonRequiredFreeText
	// reasonRequiredPartialLateMatch requires exactly partial_late_match. It is
	// the only system reason for escalated -> open.
	reasonRequiredPartialLateMatch
)

// validate applies the rule to an already-trimmed reason and returns the reason
// to persist in the audit payload.
func (r reasonRule) validate(from, to Status, reason string) (string, error) {
	if reason == "" {
		if r == reasonOptional {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s -> %s requires a reason", ErrReasonRequired, from, to)
	}
	switch r {
	case reasonRequiredPartialLateMatch:
		if reason != ReasonPartialLateMatch {
			return "", fmt.Errorf("%w: system reason for %s -> %s must be %q",
				ErrInvalidReason, from, to, ReasonPartialLateMatch)
		}
		return reason, nil
	default:
		if err := validateFreeText(reason); err != nil {
			return "", err
		}
		return reason, nil
	}
}

// validateFreeText enforces trimmed 1..500 bytes, valid UTF-8, and no NUL or
// control characters other than normal newlines and tabs. The caller trims and
// handles the empty case.
func validateFreeText(reason string) error {
	if reason == "" {
		return nil
	}
	if !utf8.ValidString(reason) {
		return fmt.Errorf("%w: reason is not valid UTF-8", ErrInvalidReason)
	}
	if len(reason) > maxReasonBytes {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrInvalidReason, maxReasonBytes)
	}
	for _, r := range reason {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: reason contains control character U+%04X", ErrInvalidReason, r)
		}
	}
	return nil
}

// validateResolution enforces the resolution shape. Empty is
// ErrResolutionRequired; a malformed value is ErrInvalidRequest because the
// value is present but not a valid resolution shape (spec 4.4).
func validateResolution(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: target %s requires a resolution type",
			ErrResolutionRequired, StatusResolved)
	}
	if len(raw) > maxResolutionBytes || !resolutionPattern.MatchString(raw) {
		return "", fmt.Errorf("%w: resolution type %q must match %s",
			ErrInvalidRequest, raw, resolutionPattern.String())
	}
	return raw, nil
}
