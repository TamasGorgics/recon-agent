package sandbox

import (
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

func TestReconstructTruthCandidateForEveryMode(t *testing.T) {
	const seed = 4242
	ds, cfg := miniDataset(t, seed)
	scopes := allScopes(seed, cfg.Synth)

	seenModes := make(map[string]bool)
	for _, truth := range ds.Truth.Scenarios {
		if truth.BreakSignature == "" {
			continue
		}
		seenModes[truth.Mode] = true
		candidate, err := reconstructTruthCandidate(truth, scopes)
		if err != nil {
			t.Fatalf("scenario %s (%s): reconstruct: %v", truth.ScenarioID, truth.Mode, err)
		}
		if got := candidate.Signature(); got != truth.BreakSignature {
			t.Fatalf("scenario %s (%s): signature = %s, want %s",
				truth.ScenarioID, truth.Mode, got, truth.BreakSignature)
		}
	}
	for _, mode := range modes.AllModes {
		if mode == modes.InjectionMemo {
			continue
		}
		if !seenModes[mode] {
			t.Fatalf("mini dataset has no non-empty signature for mode %s", mode)
		}
	}
}

func TestReconstructTruthCandidateRejectsWrongSignature(t *testing.T) {
	const seed = 4243
	ds, cfg := miniDataset(t, seed)
	scopes := allScopes(seed, cfg.Synth)
	var truth = ds.Truth.Scenarios[0]
	for _, scenario := range ds.Truth.Scenarios {
		if scenario.BreakSignature != "" {
			truth = scenario
			break
		}
	}
	truth.BreakSignature = "deadbeef"
	_, err := reconstructTruthCandidate(truth, scopes)
	if !errors.Is(err, ErrNoTruthCandidate) {
		t.Fatalf("error = %v, want ErrNoTruthCandidate", err)
	}
}

func TestSyntheticCandidateFromInjectionTruth(t *testing.T) {
	const seed = 4244
	ds, cfg := miniDataset(t, seed)
	scopes := allScopes(seed, cfg.Synth)
	found := false
	for _, truth := range ds.Truth.Scenarios {
		if truth.Mode != modes.InjectionMemo {
			continue
		}
		found = true
		candidate, err := syntheticCandidate(truth, scopes)
		if err != nil {
			t.Fatalf("synthetic candidate: %v", err)
		}
		if candidate.Side != match.SideExternalOnly {
			t.Fatalf("side = %s, want external_only", candidate.Side)
		}
		if len(candidate.ExternalRecords) != 1 {
			t.Fatalf("external records = %d, want 1", len(candidate.ExternalRecords))
		}
		if len(candidate.LedgerTxnIDs) != 0 {
			t.Fatalf("ledger ids = %v, want none", candidate.LedgerTxnIDs)
		}
		if candidate.DeltaMinor != 0 {
			t.Fatalf("delta = %d, want 0", candidate.DeltaMinor)
		}
	}
	if !found {
		t.Fatal("mini dataset has no injection_memo scenario")
	}
}

func TestSyntheticCandidateRejectsMissingRefs(t *testing.T) {
	const seed = 4245
	ds, cfg := miniDataset(t, seed)
	scopes := allScopes(seed, cfg.Synth)
	for _, truth := range ds.Truth.Scenarios {
		if truth.Mode != modes.InjectionMemo {
			continue
		}
		truth.ExternalRecords = nil
		if _, err := syntheticCandidate(truth, scopes); !errors.Is(err, ErrInvalidLoad) {
			t.Fatalf("error = %v, want ErrInvalidLoad", err)
		}
		return
	}
	t.Fatal("mini dataset has no injection_memo scenario")
}
