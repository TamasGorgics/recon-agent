package executor

import (
	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
)

// stateHashMatches compares the current canonical proposal state with the
// stored state_hash.
//
// A proposal's state_hash is computed at insertion time, when the case is
// still open or in_agent. The agent loop then moves the case to
// proposal_pending (M2-09) before approval, and a case: evidence reference
// records that proposal-time status. Those two expected transitions are not
// state drift, so when the case is now proposal_pending the comparison also
// tries the two statuses the proposal-insert contract allows. Every other
// difference (break status/delta/refs, evidence record fields, currency,
// resolution) still fails the comparison and becomes state_changed.
func stateHashMatches(current proposal.State, stored string) (bool, error) {
	if stored == "" {
		return false, nil
	}
	hashes := make([]string, 0, 3)
	for _, candidate := range append([]proposal.State{current}, preApprovalCandidates(current)...) {
		hash, err := proposal.Hash(candidate)
		if err != nil {
			return false, err
		}
		hashes = append(hashes, hash)
	}
	for _, hash := range hashes {
		if hash == stored {
			return true, nil
		}
	}
	return false, nil
}

// preApprovalCandidates reconstructs the proposal-time state for the only
// status change that happens between proposal insertion and apply. The
// proposal insert path requires the case to be open or in_agent; the caller
// applies only after the agent finalizes it to proposal_pending.
func preApprovalCandidates(current proposal.State) []proposal.State {
	if current.Case.Status != "proposal_pending" {
		return nil
	}
	return []proposal.State{
		candidateWithCaseStatus(current, "in_agent"),
		candidateWithCaseStatus(current, "open"),
	}
}

func candidateWithCaseStatus(current proposal.State, status string) proposal.State {
	candidate := current
	candidate.Case.Status = status
	candidate.Case.ResolutionType = ""
	candidate.Records = make([]proposal.RecordState, len(current.Records))
	copy(candidate.Records, current.Records)
	caseRef := "case:" + current.Case.ID
	for i := range candidate.Records {
		record := candidate.Records[i]
		if record.Kind != "case" || record.Ref != caseRef {
			continue
		}
		fields := make(map[string]any, len(record.Fields))
		for key, value := range record.Fields {
			fields[key] = value
		}
		fields["status"] = status
		fields["resolution_type"] = ""
		record.Fields = fields
		candidate.Records[i] = record
	}
	return candidate
}
