package replay

import "testing"

// TestSortDivergences proves the spec §5 order: (step, code rank, field) with a
// null field sorting before any string.
func TestSortDivergences(t *testing.T) {
	t.Parallel()
	fieldKey := "idempotency_key"
	divergences := []Divergence{
		{Code: CodeProposalFieldDiff, Step: 0, Field: &fieldKey},
		{Code: CodeProposalDigestMismatch, Step: 0},
		{Code: CodeToolResultMismatch, Step: 2},
		{Code: CodeToolCallUnrecorded, Step: 2},
		{Code: CodeRequestHashMismatch, Step: 2},
		{Code: CodeStepCountMismatch, Step: 1},
	}
	sortDivergences(divergences)
	want := []string{
		CodeProposalDigestMismatch,
		CodeProposalFieldDiff,
		CodeStepCountMismatch,
		CodeRequestHashMismatch,
		CodeToolCallUnrecorded,
		CodeToolResultMismatch,
	}
	for i, code := range want {
		if divergences[i].Code != code {
			t.Fatalf("divergence %d = %+v, want %s (order %+v)", i, divergences[i], code, divergences)
		}
	}
}

// TestReportFinalizeKeyOnlyDiff proves a key-only proposal_field_diff is not
// identity-breaking while any other divergence is.
func TestReportFinalizeKeyOnlyDiff(t *testing.T) {
	t.Parallel()
	key := "idempotency_key"
	keyOnly := &Report{
		ProposalHash: "same", StoredHash: "same",
		Divergences: []Divergence{{Code: CodeProposalFieldDiff, Step: 0, Field: &key}},
	}
	keyOnly.finalize()
	if !keyOnly.Identical {
		t.Fatal("key-only diff changed identical")
	}

	breaking := &Report{
		ProposalHash: "same", StoredHash: "same",
		Divergences: []Divergence{{Code: CodeProposalDigestMismatch, Step: 0}},
	}
	breaking.finalize()
	if breaking.Identical {
		t.Fatal("proposal_digest_mismatch did not change identical")
	}

	noProposal := &Report{ProposalHash: "", StoredHash: "same"}
	noProposal.finalize()
	if noProposal.Identical {
		t.Fatal("an unbuilt proposal hash was reported identical")
	}
}
