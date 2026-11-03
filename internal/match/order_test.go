//go:build integration

// covers: FIN-42

package match_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
)

// TestEvaluateIsOrderIndependent shuffles the candidate input and asserts the
// same canonical set of match groups comes out. The engine calls the same
// Evaluate path before inserting anything.
func TestEvaluateIsOrderIndependent(t *testing.T) {
	scope := match.Scope{ID: "scope-1", LedgerAccountID: "account-1", Source: "psp_a", Currency: "USD"}
	window := match.Window{From: mustDate(t, "2026-06-01"), To: mustDate(t, "2026-06-30")}
	now := mustTime(t, testNow)

	external := []match.ExternalRecord{
		{ID: "e-exact", Source: "psp_a", SourceRecordID: "sr-1", AmountMinor: 10_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-10"), Reference: "EXACT"},
		{ID: "e-tol", Source: "psp_a", SourceRecordID: "sr-2", AmountMinor: 20_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-11"), Reference: "TOL"},
		{ID: "e-pre", Source: "psp_a", SourceRecordID: "sr-3", AmountMinor: 30_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-12"), Reference: "PRE"},
		{ID: "e-dup-a", Source: "psp_a", SourceRecordID: "sr-4", AmountMinor: 40_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-13"), Reference: "DUP"},
		{ID: "e-dup-b", Source: "psp_a", SourceRecordID: "sr-5", AmountMinor: 40_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-13"), Reference: "DUP"},
	}
	ledger := []match.LedgerTxn{
		{ID: "l-exact", AmountMinor: 10_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-10"), Reference: "EXACT"},
		{ID: "l-tol", AmountMinor: 19_950, Currency: "USD", ValueDate: mustDate(t, "2026-06-11"), Reference: "TOL"},
		{ID: "l-pre-exact", AmountMinor: 30_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-12"), Reference: "PRE"},
		{ID: "l-pre-tol", AmountMinor: 29_950, Currency: "USD", ValueDate: mustDate(t, "2026-06-12"), Reference: "PRE"},
		{ID: "l-single-dup", AmountMinor: 40_000, Currency: "USD", ValueDate: mustDate(t, "2026-06-13"), Reference: "DUP"},
	}

	passes := match.DefaultPasses(config.MatchConfig{FeeToleranceMinor: map[string]int64{"USD": 100}})
	forward, forwardCounts, err := match.Evaluate(context.Background(),
		match.NewRunContext(scope, window, now, external, ledger), passes)
	if err != nil {
		t.Fatalf("evaluate forward: %v", err)
	}
	backwardExternal := append([]match.ExternalRecord(nil), external...)
	backwardLedger := append([]match.LedgerTxn(nil), ledger...)
	sort.SliceStable(backwardExternal, func(i, j int) bool { return backwardExternal[i].ID > backwardExternal[j].ID })
	sort.SliceStable(backwardLedger, func(i, j int) bool { return backwardLedger[i].ID > backwardLedger[j].ID })
	backward, backwardCounts, err := match.Evaluate(context.Background(),
		match.NewRunContext(scope, window, now, backwardExternal, backwardLedger), passes)
	if err != nil {
		t.Fatalf("evaluate backward: %v", err)
	}

	wantForward := canonicalCandidates(forward)
	wantBackward := canonicalCandidates(backward)
	if strings.Join(wantForward, "\n") != strings.Join(wantBackward, "\n") {
		t.Fatalf("match sets differ under shuffled input:\nforward:\n%s\nbackward:\n%s",
			strings.Join(wantForward, "\n"), strings.Join(wantBackward, "\n"))
	}
	if len(forwardCounts) != len(backwardCounts) {
		t.Fatalf("pass counts differ: %v vs %v", forwardCounts, backwardCounts)
	}
	for name, count := range forwardCounts {
		if backwardCounts[name] != count {
			t.Fatalf("pass %q count = %d, want %d", name, backwardCounts[name], count)
		}
	}
	if got, want := len(wantForward), 3; got != want {
		t.Fatalf("canonical matches = %d (%v), want %d", got, wantForward, want)
	}
	if got, want := forwardCounts["exact"], 2; got != want {
		t.Errorf("exact count = %d, want %d (EXACT and PRE)", got, want)
	}
	if got, want := forwardCounts["reference_tolerance"], 1; got != want {
		t.Errorf("tolerance count = %d, want %d (TOL; PRE is already claimed)", got, want)
	}
}

func canonicalCandidates(candidates []match.Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		legs := make([]string, 0, len(candidate.Legs))
		for _, leg := range candidate.Legs {
			legs = append(legs, leg.LedgerTxnID)
		}
		sort.Strings(legs)
		out = append(out, strings.Join([]string{
			candidate.External.ID,
			strings.Join(legs, ","),
			candidate.Rule,
		}, "|"))
	}
	sort.Strings(out)
	return out
}
