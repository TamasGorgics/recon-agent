package replay

import (
	"sort"
	"time"
)

// Divergence codes from spec §5. The order of the constants is the tie-break
// rank.
const (
	CodeRequestHashMismatch    = "request_hash_mismatch"
	CodeToolCallUnrecorded     = "tool_call_unrecorded"
	CodeToolResultMismatch     = "tool_result_mismatch"
	CodeStepCountMismatch      = "step_count_mismatch"
	CodeProposalDigestMismatch = "proposal_digest_mismatch"
	CodeProposalFieldDiff      = "proposal_field_diff"
)

// CodeToolResultHashUnavailable is a diagnostic, not a divergence: it marks a
// legacy tool_calls row recorded before result_canonical_hash existed.
const CodeToolResultHashUnavailable = "tool_result_hash_unavailable"

// CodeStateHashInvalid is a diagnostic: the stored state_hash is present but is
// not 64 lowercase hex characters. state_hash never affects identity.
const CodeStateHashInvalid = "state_hash_invalid"

var divergenceRank = map[string]int{
	CodeRequestHashMismatch:    1,
	CodeToolCallUnrecorded:     2,
	CodeToolResultMismatch:     3,
	CodeStepCountMismatch:      4,
	CodeProposalDigestMismatch: 5,
	CodeProposalFieldDiff:      6,
}

// Divergence is one difference between the recording and the reconstruction.
// Step is the 1-based llm_calls.seq; 0 is reserved for run- and
// proposal-level entries.
type Divergence struct {
	Code   string  `json:"code"`
	Step   int     `json:"step"`
	Field  *string `json:"field"`
	Detail *string `json:"detail"`
}

// Diagnostic is non-identity-breaking metadata. It is reported outside the
// divergences array so a faithful legacy run can still be identical.
type Diagnostic struct {
	Code   string `json:"code"`
	Step   int    `json:"step"`
	Detail string `json:"detail"`
}

// ClockEvent is one timestamp and recorded duration fed into the fake replay
// clock. It is exposed so callers (and tests) can prove replay used recorded
// time, not wall time.
type ClockEvent struct {
	Kind       string    `json:"kind"`
	Step       int       `json:"step"`
	Tool       int       `json:"tool,omitempty"`
	At         time.Time `json:"at"`
	DurationMS int       `json:"duration_ms"`
}

// Report is the replay result. The required JSON keys are RunID, Mode,
// Identical, ProposalHash, StoredHash and Divergences; the rest is diagnostic
// metadata.
type Report struct {
	RunID        string       `json:"run_id"`
	Mode         string       `json:"mode"`
	Identical    bool         `json:"identical"`
	ProposalHash string       `json:"proposal_hash"`
	StoredHash   string       `json:"stored_hash"`
	Divergences  []Divergence `json:"divergences"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
	StateHash    string       `json:"state_hash,omitempty"`
	Outcome      string       `json:"outcome,omitempty"`
	StepsTotal   int          `json:"steps_total,omitempty"`
	Model        string       `json:"model,omitempty"`
	Prompt       string       `json:"prompt,omitempty"`
	Clock        []ClockEvent `json:"clock,omitempty"`
}

// addDivergence appends one divergence, allocating the field/detail pointers so
// JSON renders null rather than omitting the keys.
func (r *Report) addDivergence(code string, step int, field, detail string) {
	var fieldPtr *string
	if field != "" {
		value := field
		fieldPtr = &value
	}
	var detailPtr *string
	if detail != "" {
		value := detail
		detailPtr = &value
	}
	if r.Divergences == nil {
		r.Divergences = make([]Divergence, 0, 1)
	}
	r.Divergences = append(r.Divergences, Divergence{
		Code: code, Step: step, Field: fieldPtr, Detail: detailPtr,
	})
}

// sortDivergences applies the spec §5 order: (step, code rank, field) with a
// null field before any string. Detail is a final deterministic tie-break for
// entries that would otherwise compare equal.
func sortDivergences(divergences []Divergence) {
	sort.SliceStable(divergences, func(i, j int) bool {
		a, b := divergences[i], divergences[j]
		if a.Step != b.Step {
			return a.Step < b.Step
		}
		rankA, okA := divergenceRank[a.Code]
		if !okA {
			rankA = len(divergenceRank) + 1
		}
		rankB, okB := divergenceRank[b.Code]
		if !okB {
			rankB = len(divergenceRank) + 1
		}
		if rankA != rankB {
			return rankA < rankB
		}
		if fieldA, fieldB := fieldValue(a.Field), fieldValue(b.Field); fieldA != fieldB {
			if a.Field == nil {
				return true
			}
			if b.Field == nil {
				return false
			}
			return fieldA < fieldB
		}
		return detailValue(a.Detail) < detailValue(b.Detail)
	})
}

func fieldValue(field *string) string {
	if field == nil {
		return ""
	}
	return *field
}

func detailValue(detail *string) string {
	if detail == nil {
		return ""
	}
	return *detail
}

// isIdentityBreaking reports whether one divergence changes `identical`. A
// key-only proposal_field_diff is deliberately non-identity-breaking per the
// approved spec decision; every other code is breaking.
func isIdentityBreaking(d Divergence) bool {
	if d.Code == CodeProposalFieldDiff && d.Field != nil && *d.Field == "idempotency_key" {
		return false
	}
	return true
}

// finalize sorts the divergence list and computes the verdict. The report is
// identical only when both proposal digests agree and no identity-breaking
// divergence exists; diagnostics never affect the verdict.
func (r *Report) finalize() {
	sortDivergences(r.Divergences)
	breaking := false
	for _, divergence := range r.Divergences {
		if isIdentityBreaking(divergence) {
			breaking = true
			break
		}
	}
	r.Identical = !breaking && r.ProposalHash != "" && r.ProposalHash == r.StoredHash
}
