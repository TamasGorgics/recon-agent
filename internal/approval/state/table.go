package state

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/TamasGorgics/recon-agent/internal/audit"
)

// maxReasonRunes bounds the audit-only transition reason. Longer reasons are
// rejected rather than truncated so a caller cannot silently lose context.
const maxReasonRunes = 2000

// applyFailedReasons is the exact apply_failed reason enum from the roadmap.
var applyFailedReasons = []string{"state_changed", "break_resolved", "expired"}

// Edge is one legal proposal status transition. Actor is the only permitted
// actor type for the edge; roles and four-eyes are enforced by M4-11.
type Edge struct {
	From           Status
	To             Status
	Actor          audit.ActorType
	ReasonRequired bool
	// ReasonEnum is non-empty only for apply_failed edges.
	ReasonEnum []string
	Purpose    string
}

// edges is the complete, canonical transition table. It is ordered by source
// status (spec order) and then target status; Edges keeps that order and the
// markdown renderer relies on it.
var edges = []Edge{
	{StatusProposed, StatusPendingApproval, audit.ActorSystem, false, nil,
		"M4-11 Request routes T1/T2 to approval"},
	{StatusProposed, StatusAutoApproved, audit.ActorSystem, false, nil,
		"M4-11 T0 path; policy decision owned by M4-04"},
	{StatusProposed, StatusExpired, audit.ActorSystem, false, nil,
		"ExpireDue expires a proposal that was never routed"},
	{StatusProposed, StatusSuperseded, audit.ActorSystem, true, nil,
		"newer proposal or a late match makes it moot"},
	{StatusPendingApproval, StatusApproved, audit.ActorHuman, false, nil,
		"M4-11 Decide; FIN-33 forbids agent/system approval"},
	{StatusPendingApproval, StatusRejected, audit.ActorHuman, true, nil,
		"M4-11 Decide; rejection reason feeds evals"},
	{StatusPendingApproval, StatusExpired, audit.ActorSystem, false, nil,
		"ExpireDue for a pending proposal"},
	{StatusPendingApproval, StatusSuperseded, audit.ActorSystem, true, nil,
		"newer proposal or a late match makes it moot"},
	{StatusApproved, StatusApplied, audit.ActorSystem, false, nil,
		"M4-06 atomic apply"},
	{StatusApproved, StatusApplyFailed, audit.ActorSystem, true, applyFailedReasons,
		"M4-06 apply failure; enum state_changed/break_resolved/expired"},
	{StatusApproved, StatusSuperseded, audit.ActorSystem, true, nil,
		"late match makes the approved proposal moot"},
	{StatusAutoApproved, StatusApplied, audit.ActorSystem, false, nil,
		"M4-06 atomic apply"},
	{StatusAutoApproved, StatusApplyFailed, audit.ActorSystem, true, applyFailedReasons,
		"M4-06 apply failure; enum state_changed/break_resolved/expired"},
	{StatusAutoApproved, StatusSuperseded, audit.ActorSystem, true, nil,
		"late match or newer proposal makes it moot"},
	{StatusApplyFailed, StatusApproved, audit.ActorSystem, false, nil,
		"retry edge; M4-06 owns retry policy and re-validation"},
	{StatusApplyFailed, StatusAutoApproved, audit.ActorSystem, false, nil,
		"retry edge for a former T0 proposal; M4-06 owns policy"},
	{StatusApplyFailed, StatusSuperseded, audit.ActorSystem, true, nil,
		"non-terminal supersession"},
}

// Edges returns the 17 legal edges sorted by From then To in spec status order.
// The slice is a copy; callers cannot mutate the table.
func Edges() []Edge {
	out := make([]Edge, len(edges))
	for i, edge := range edges {
		out[i] = edge
		out[i].ReasonEnum = cloneStrings(edge.ReasonEnum)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return statusRank(out[i].From) < statusRank(out[j].From)
		}
		return statusRank(out[i].To) < statusRank(out[j].To)
	})
	return out
}

// Lookup returns the legal edge from -> to, if any.
func Lookup(from, to Status) (Edge, bool) {
	for _, edge := range edges {
		if edge.From == from && edge.To == to {
			edge.ReasonEnum = cloneStrings(edge.ReasonEnum)
			return edge, true
		}
	}
	return Edge{}, false
}

// AllowsActor reports whether an actor of type t may perform the edge.
func (e Edge) AllowsActor(t audit.ActorType) bool {
	return e.Actor == t
}

// ValidateReason applies the reason rules for the edge: required on
// rejected/apply_failed/superseded, optional elsewhere; trimmed; at most 2000
// runes; no control characters; apply_failed restricted to its exact enum.
func (e Edge) ValidateReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		if e.ReasonRequired {
			return fmt.Errorf("%w: %s -> %s requires a reason", ErrReasonRequired, e.From, e.To)
		}
		return nil
	}
	if len([]rune(trimmed)) > maxReasonRunes {
		return fmt.Errorf("%w: reason exceeds %d runes", ErrInvalidReason, maxReasonRunes)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: reason contains control character U+%04X", ErrInvalidReason, r)
		}
	}
	if len(e.ReasonEnum) == 0 {
		return nil
	}
	for _, allowed := range e.ReasonEnum {
		if trimmed == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: reason %q is not one of %s",
		ErrInvalidReason, trimmed, strings.Join(e.ReasonEnum, ", "))
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}
