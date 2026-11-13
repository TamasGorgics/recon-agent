package verify

import "sort"

// VerifyReferenceTypo requires exactly one eligible ledger candidate for a
// reference-typo link and requires the proposal's cited ledger set to be that
// candidate.
//
// Candidates is the service-scoped pool: same reconciliation scope/source as
// the external record and not already matched to another external record. The
// pure evaluator applies the remaining exact amount, currency, and inclusive
// absolute value-date window checks. The window anchors on External.ValueDate
// when set, falling back to External.OccurredAt. A second eligible candidate
// always fails, regardless of how similar the reference strings are. The
// reference string itself is never compared: a typo is the reason the record
// did not match, not a value to verify.
func VerifyReferenceTypo(in ReferenceTypoInput, opts Options) Result {
	details := Details{Currency: in.External.Currency}
	if r, ok := checkClaim(in.Claimed, PatternReferenceTypo, details); !ok {
		return r
	}
	opts, err := resolveOptions(opts)
	if err != nil {
		return failure(PatternReferenceTypo, in.Claimed, details, ReasonInvalidOptions)
	}

	var reasons []string
	if in.External.Source == "" || in.External.SourceRecordID == "" {
		reasons = append(reasons, ReasonSourceAmbiguous)
	}
	if !validCurrency(in.External.Currency) {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if in.External.AmountMinor <= 0 {
		reasons = append(reasons, ReasonInvalidInput)
	}
	proposed := trimNonEmpty(in.ProposedLedgerIDs)
	if len(proposed) == 0 {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if referenceDate(in.External).IsZero() {
		reasons = append(reasons, ReasonInvalidInput)
	}
	if len(reasons) > 0 {
		return failure(PatternReferenceTypo, in.Claimed, details, reasons...)
	}

	eligible := make([]Candidate, 0, len(in.Candidates))
	for _, candidate := range in.Candidates {
		if candidate.LedgerTxnID == "" || !validCurrency(candidate.Currency) || candidate.ValueDate.IsZero() {
			return failure(PatternReferenceTypo, in.Claimed, details, ReasonInvalidInput)
		}
		if candidate.Currency != in.External.Currency {
			continue
		}
		if candidate.AmountMinor != in.External.AmountMinor {
			continue
		}
		if !withinDateWindow(referenceDate(in.External), candidate.ValueDate, opts.ReferenceWindowDays) {
			continue
		}
		eligible = append(eligible, candidate)
	}
	// Candidate ordering is by ledger txn ID so diagnostics are deterministic
	// even though this schema version only records the count.
	sort.Slice(eligible, func(i, j int) bool {
		return eligible[i].LedgerTxnID < eligible[j].LedgerTxnID
	})
	details.Candidates = len(eligible)

	switch len(eligible) {
	case 0:
		return failure(PatternReferenceTypo, in.Claimed, details, ReasonNoCandidate)
	case 1:
		candidate := eligible[0]
		details.ExpectedMinor = in.External.AmountMinor
		details.ActualMinor = candidate.AmountMinor
		if len(proposed) != 1 {
			return failure(PatternReferenceTypo, in.Claimed, details, ReasonCandidateMismatch)
		}
		if _, ok := proposed[candidate.LedgerTxnID]; !ok {
			return failure(PatternReferenceTypo, in.Claimed, details, ReasonCandidateMismatch)
		}
		return success(PatternReferenceTypo, in.Claimed, details)
	default:
		return failure(PatternReferenceTypo, in.Claimed, details, ReasonMultipleCandidates)
	}
}
