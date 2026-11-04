package match_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// Unit-test helpers shared by the M1-13 pass tests. Integration helpers live in
// helpers_test.go behind the integration build tag; the names here deliberately
// differ so both sets compile together under -tags=integration.

var (
	unitScope = match.Scope{
		ID:              "scope-1",
		LedgerAccountID: "account-1",
		Source:          "psp_a",
		Currency:        "USD",
	}
	unitWindow = match.Window{From: uDate("2026-06-01"), To: uDate("2026-06-30")}
)

func uDate(value string) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		panic(err)
	}
	return parsed
}

func uTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}

func unitConfig() config.MatchConfig {
	return config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": 100}}
}

func evaluatePasses(t *testing.T, cfg config.MatchConfig, external []match.ExternalRecord, ledger []match.LedgerTxn) ([]match.Candidate, []match.BreakCandidate, map[string]int) {
	t.Helper()
	candidates, breaks, counts, err := match.EvaluateWithBreaks(context.Background(),
		match.NewRunContext(unitScope, unitWindow, uTime("2026-06-15T12:00:00Z"), external, ledger),
		match.DefaultPasses(cfg))
	if err != nil {
		t.Fatalf("evaluate passes: %v", err)
	}
	return candidates, breaks, counts
}

func findCandidate(candidates []match.Candidate, externalID string) (match.Candidate, bool) {
	for _, candidate := range candidates {
		if candidate.External.ID == externalID {
			return candidate, true
		}
	}
	return match.Candidate{}, false
}

func legIDs(candidate match.Candidate) []string {
	ids := make([]string, 0, len(candidate.Legs))
	for _, leg := range candidate.Legs {
		ids = append(ids, leg.LedgerTxnID)
	}
	sort.Strings(ids)
	return ids
}

func findBreaks(breaks []match.BreakCandidate, side match.BreakSide) []match.BreakCandidate {
	var out []match.BreakCandidate
	for _, candidate := range breaks {
		if candidate.Side == side {
			out = append(out, candidate)
		}
	}
	return out
}

// canonicalRun renders both matches and breaks into sorted, comparable lines so
// order-independence assertions cover the whole pass chain, not only matches.
func canonicalRun(candidates []match.Candidate, breaks []match.BreakCandidate) []string {
	out := make([]string, 0, len(candidates)+len(breaks))
	for _, candidate := range candidates {
		out = append(out, "match|"+candidate.External.ID+"|"+strings.Join(legIDs(candidate), ",")+"|"+candidate.Rule)
	}
	for _, candidate := range breaks {
		refs := make([]string, 0, len(candidate.ExternalRecords))
		for _, ref := range candidate.ExternalRecords {
			refs = append(refs, ref.Source+":"+ref.SourceRecordID)
		}
		sort.Strings(refs)
		out = append(out, "break|"+string(candidate.Side)+"|"+strings.Join(refs, ",")+"|"+candidate.Signature())
	}
	sort.Strings(out)
	return out
}
