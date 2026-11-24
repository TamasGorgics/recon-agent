package demo

import (
	"bytes"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func testOpsPlan(seed int64, now time.Time) opsPlan {
	return buildOpsPlan(seed, now, opsPlanContext{
		PrimaryCurrency:   "USD",
		SecondaryCurrency: "EUR",
		PrimarySource:     "psp_a",
		SecondarySource:   "bank_x",
	})
}

func TestBuildOpsPlanCoverage(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	plan := testOpsPlan(1, now)
	summary := plan.summary(Summary{Seed: 1})

	if got, want := summary.AgentRuns, 8; got != want {
		t.Errorf("agent runs = %d, want %d", got, want)
	}
	if got, want := summary.Approvers, 2; got != want {
		t.Errorf("approvers = %d, want %d", got, want)
	}
	if got, want := summary.Proposals, 8; got != want {
		t.Errorf("proposals = %d, want %d", got, want)
	}
	if got, want := summary.Approvals, 4; got != want {
		t.Errorf("approvals = %d, want %d", got, want)
	}
	if got, want := summary.Applied, 5; got != want {
		t.Errorf("applied proposals = %d, want %d", got, want)
	}
	if got, want := summary.Cases, 9; got != want {
		t.Errorf("cases = %d, want %d", got, want)
	}
	if got, want := summary.ResolvedHuman, 2; got != want {
		t.Errorf("resolved human = %d, want %d", got, want)
	}
	if got, want := summary.ResolvedAuto, 2; got != want {
		t.Errorf("resolved auto = %d, want %d", got, want)
	}
	if got, want := summary.Escalated, 2; got != want {
		t.Errorf("escalated = %d, want %d", got, want)
	}
	if got, want := summary.PendingCases, 2; got != want {
		t.Errorf("pending cases = %d, want %d", got, want)
	}
	if got, want := summary.Breaks, 8; got != want {
		t.Errorf("breaks = %d, want %d", got, want)
	}
	if got, want := summary.ExternalRecords, 7; got != want {
		t.Errorf("external records = %d, want %d", got, want)
	}
	if got, want := summary.LedgerTxns, 1; got != want {
		t.Errorf("ledger txns = %d, want %d", got, want)
	}

	wantBuckets := map[string]int{"0-1": 1, "2-3": 2, "4-7": 1, ">7": 2, "unknown": 2}
	if !reflect.DeepEqual(summary.AgingBuckets, wantBuckets) {
		t.Errorf("aging buckets = %v, want %v", summary.AgingBuckets, wantBuckets)
	}
	wantClasses := map[string]int{"auto": 2, "human": 2, "escalated": 2}
	if !reflect.DeepEqual(summary.Classifications, wantClasses) {
		t.Errorf("classifications = %v, want %v", summary.Classifications, wantClasses)
	}

	statuses := map[string]int{}
	tiers := map[string]int{}
	for _, proposal := range plan.Proposals {
		statuses[proposal.Status]++
		tiers[proposal.RiskTier]++
	}
	wantStatuses := map[string]int{"applied": 5, "pending_approval": 2, "rejected": 1}
	if !reflect.DeepEqual(statuses, wantStatuses) {
		t.Errorf("proposal statuses = %v, want %v", statuses, wantStatuses)
	}
	if tiers["T0"] == 0 || tiers["T1"] == 0 {
		t.Errorf("risk tiers = %v, want T0 and T1 coverage", tiers)
	}

	decisions := map[string]int{}
	for _, approval := range plan.Approvals {
		decisions[approval.Decision]++
	}
	wantDecisions := map[string]int{"approved": 3, "rejected": 1}
	if !reflect.DeepEqual(decisions, wantDecisions) {
		t.Errorf("approval decisions = %v, want %v", decisions, wantDecisions)
	}

	// The two-ref min rule and both unknown shapes are plan-level invariants.
	for _, breakRow := range plan.Breaks {
		switch breakRow.Key {
		case "break-multi-ref":
			want := plan.Anchor.AddDate(0, 0, -60)
			if breakRow.MinValueDate == nil || !breakRow.MinValueDate.Equal(want) {
				t.Errorf("multi-ref min value date = %v, want %v", breakRow.MinValueDate, want)
			}
		case "break-unknown-ref":
			if breakRow.MinValueDate != nil {
				t.Errorf("unknown ref min value date = %v, want nil", breakRow.MinValueDate)
			}
		case "break-future-ref":
			want := plan.Anchor.AddDate(0, 0, 5)
			if breakRow.MinValueDate == nil || !breakRow.MinValueDate.Equal(want) {
				t.Errorf("future ref value date = %v, want %v", breakRow.MinValueDate, want)
			}
		}
	}

	// Escalated > human: the precedence case has an approved and applied
	// proposal but still classifies escalated.
	var precedenceCase opsCase
	for _, c := range plan.Cases {
		if c.Key == "case-escalated-1" {
			precedenceCase = c
			break
		}
	}
	if precedenceCase.ID == "" {
		t.Fatal("precedence case is missing from the plan")
	}
	var precedenceProposal opsProposal
	for _, proposal := range plan.Proposals {
		if proposal.CaseID == precedenceCase.ID {
			precedenceProposal = proposal
		}
	}
	if precedenceProposal.ID == "" {
		t.Fatal("precedence proposal is missing from the plan")
	}
	approved := false
	for _, approval := range plan.Approvals {
		if approval.ProposalID == precedenceProposal.ID && approval.Decision == "approved" {
			approved = true
		}
	}
	if !approved {
		t.Fatal("precedence case has no approved proposal")
	}
	applied := false
	for _, row := range plan.Applied {
		if row.ProposalID == precedenceProposal.ID {
			applied = true
		}
	}
	if !applied {
		t.Fatal("precedence case has no applied proposal")
	}

	signatures := map[string]bool{}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, breakRow := range plan.Breaks {
		if !hex64.MatchString(breakRow.Signature) {
			t.Errorf("break %s signature %q is not 64 lowercase hex", breakRow.Key, breakRow.Signature)
		}
		if signatures[breakRow.Signature] {
			t.Errorf("duplicate break signature %q", breakRow.Signature)
		}
		signatures[breakRow.Signature] = true
	}
}

func TestBuildOpsPlanDeterministic(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	first := testOpsPlan(7, now)
	second := testOpsPlan(7, now)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed and clock produced different plans")
	}
	otherSeed := testOpsPlan(8, now)
	if first.Cases[0].ID == otherSeed.Cases[0].ID {
		t.Fatal("different seeds produced the same case ID")
	}
	if first.Breaks[0].Signature == otherSeed.Breaks[0].Signature {
		t.Fatal("different seeds produced the same break signature")
	}

	// Day-anchored values are stable across the UTC day even though pending
	// proposal timestamps use the clock.
	evening := testOpsPlan(7, time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC))
	if !reflect.DeepEqual(first.summary(Summary{}), evening.summary(Summary{})) {
		t.Fatal("same-day plans produced different coverage summaries")
	}
	nextDay := testOpsPlan(7, now.AddDate(0, 0, 1))
	if first.summary(Summary{}).Anchor == nextDay.summary(Summary{}).Anchor {
		t.Fatal("next-day plan kept the previous anchor")
	}
}

func TestOpsSummaryWriteCountsOnly(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	summary := testOpsPlan(1, now).summary(Summary{Seed: 1, Days: 14, Difficulty: "realistic"})
	var out bytes.Buffer
	if err := summary.Write(&out); err != nil {
		t.Fatalf("Write: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"M5-04 ops demo summary: anchor=2026-11-24",
		"agent_runs=8 approvers=2 proposals=8 approvals=4 applied=5",
		"cases=9 resolved_human=2 resolved_auto=2 escalated=2 pending=2",
		"breaks=8 external_records=7 ledger_txns=1",
		"aging 0-1=1 2-3=2 4-7=1 >7=2 unknown=2",
		"classification auto=2 escalated=2 human=2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary does not contain %q:\n%s", want, text)
		}
	}
	uuidPattern := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	if uuidPattern.MatchString(text) {
		t.Errorf("summary leaks a UUID:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "postgres://") || strings.Contains(strings.ToLower(text), "password") {
		t.Errorf("summary leaks a DSN or secret:\n%s", text)
	}
}

func TestOpsAgingBucket(t *testing.T) {
	anchor := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	at := func(days int) *time.Time {
		value := anchor.AddDate(0, 0, days)
		return &value
	}
	cases := []struct {
		days   int
		bucket string
		order  int
	}{
		{0, "0-1", 1},
		{1, "0-1", 1},
		{2, "2-3", 2},
		{3, "2-3", 2},
		{4, "4-7", 3},
		{7, "4-7", 3},
		{8, ">7", 4},
		{60, ">7", 4},
	}
	for _, tc := range cases {
		gotBucket, gotOrder := opsAgingBucket(anchor, at(-tc.days))
		if gotBucket != tc.bucket || gotOrder != tc.order {
			t.Errorf("age %d days = %s/%d, want %s/%d", tc.days, gotBucket, gotOrder, tc.bucket, tc.order)
		}
	}
	if bucket, order := opsAgingBucket(anchor, at(5)); bucket != "unknown" || order != 5 {
		t.Errorf("future date = %s/%d, want unknown/5", bucket, order)
	}
	if bucket, order := opsAgingBucket(anchor, nil); bucket != "unknown" || order != 5 {
		t.Errorf("missing date = %s/%d, want unknown/5", bucket, order)
	}
}

func TestErrOpsDataPresentMessage(t *testing.T) {
	want := "demo ops: ops data already present; rerun with --reset"
	if ErrOpsDataPresent.Error() != want {
		t.Errorf("error = %q, want %q", ErrOpsDataPresent, want)
	}
	if !errors.Is(ErrOpsDataPresent, ErrOpsDataPresent) {
		t.Error("ErrOpsDataPresent is not errors.Is-compatible")
	}
}
