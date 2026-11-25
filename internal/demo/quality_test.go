package demo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func testQualityPlan(seed int64, now time.Time) qualityPlan {
	return buildQualityPlan(seed, now)
}

func TestBuildQualityPlanDeterministic(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	first := testQualityPlan(1, now)
	second := testQualityPlan(1, now)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("buildQualityPlan is not deterministic for the same seed and clock")
	}
	other := testQualityPlan(2, now)
	if reflect.DeepEqual(first, other) {
		t.Fatal("buildQualityPlan produced identical IDs for different seeds")
	}
}

func TestQualityPlanCoverage(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	plan := testQualityPlan(1, now)
	summary := plan.summary(nil)

	if got, want := len(plan.EvalRuns), 13; got != want {
		t.Fatalf("eval runs = %d, want %d", got, want)
	}
	if got, want := plan.EvalResult, 64; got != want {
		t.Fatalf("eval case results = %d, want %d", got, want)
	}
	for _, run := range plan.EvalRuns {
		if got := len(run.Cases); got < 4 || got > 8 {
			t.Errorf("run %s case results = %d, want 4..8", run.Key, got)
		}
	}

	if got, want := summary.EvalRuns, 13; got != want {
		t.Errorf("summary eval runs = %d, want %d", got, want)
	}
	if got, want := summary.EvalCaseResult, 64; got != want {
		t.Errorf("summary eval case results = %d, want %d", got, want)
	}
	if got, want := summary.EvalDays, 7; got != want {
		t.Errorf("summary eval days = %d, want %d", got, want)
	}
	if got, want := summary.Splits["dev"], 4; got != want {
		t.Errorf("dev split runs = %d, want %d (summary %v)", got, want, summary.Splits)
	}
	if got, want := summary.Splits["test"], 7; got != want {
		t.Errorf("test split runs = %d, want %d (summary %v)", got, want, summary.Splits)
	}
	if got, want := summary.Splits["injection"], 2; got != want {
		t.Errorf("injection split runs = %d, want %d (summary %v)", got, want, summary.Splits)
	}
	if got, want := summary.Suites["agent"], 9; got != want {
		t.Errorf("agent suite runs = %d, want %d (summary %v)", got, want, summary.Suites)
	}
	if got, want := summary.Suites["e2e"], 3; got != want {
		t.Errorf("e2e suite runs = %d, want %d (summary %v)", got, want, summary.Suites)
	}
	if got, want := summary.Suites["matcher"], 1; got != want {
		t.Errorf("matcher suite runs = %d, want %d (summary %v)", got, want, summary.Suites)
	}

	// Every model/prompt pair has at least one non-injection run so the
	// accuracy series is populated for all four dimensions.
	accuracyCombos := map[string]bool{}
	for _, run := range plan.EvalRuns {
		if run.Split != "injection" && run.Suite != "matcher" && run.EndedAt != nil && run.RepeatGroup == nil {
			accuracyCombos[run.Model+"/"+run.PromptVersion] = true
		}
	}
	for _, model := range plan.Models {
		for _, prompt := range plan.Prompts {
			if !accuracyCombos[model+"/"+prompt] {
				t.Errorf("no non-injection completed run for %s/%s", model, prompt)
			}
		}
	}

	// At least four distinct UTC days within the last seven days, including
	// the anchor day. Every fixture day is derived from the anchor.
	days := map[string]bool{}
	for _, run := range plan.EvalRuns {
		start := run.StartedAt.UTC()
		if start.Before(plan.Anchor.AddDate(0, 0, -7)) || start.After(plan.Anchor.AddDate(0, 0, 1)) {
			t.Errorf("eval run %s started at %s, outside the anchor-7d window", run.Key, start)
		}
		days[start.Format("2006-01-02")] = true
	}
	if len(days) < 4 {
		t.Errorf("eval days = %d, want at least 4", len(days))
	}
	if !days[plan.Anchor.Format("2006-01-02")] {
		t.Errorf("eval runs do not include the anchor day %s", plan.Anchor.Format("2006-01-02"))
	}
}

func TestQualityPlanExclusionRows(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	plan := testQualityPlan(1, now)

	var repeat, unfinished, matcher bool
	for _, run := range plan.EvalRuns {
		switch run.Key {
		case "excluded-repeat":
			repeat = run.RepeatGroup != nil
		case "excluded-unfinished":
			unfinished = run.EndedAt == nil
		case "excluded-matcher":
			matcher = run.Suite == "matcher"
		}
		for _, result := range run.Cases {
			if (run.Key == "excluded-repeat" || run.Key == "excluded-unfinished" || run.Key == "excluded-matcher") &&
				result.CostUSD != "777.000000" {
				t.Errorf("exclusion run %s case cost = %s, want the distinctive 777.000000", run.Key, result.CostUSD)
			}
		}
	}
	if !repeat || !unfinished || !matcher {
		t.Fatalf("exclusion rows missing: repeat=%t unfinished=%t matcher=%t", repeat, unfinished, matcher)
	}

	var injectionDistinct bool
	for _, run := range plan.EvalRuns {
		if run.Split != "injection" {
			continue
		}
		for _, result := range run.Cases {
			if result.CostUSD == "0.777777" && result.LatencyMS == 9001 {
				injectionDistinct = true
			}
		}
	}
	if !injectionDistinct {
		t.Fatal("injection fixture lacks the distinctive 0.777777 / 9001 case")
	}
}

func TestQualityPlanProductionCoverage(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	plan := testQualityPlan(1, now)
	summary := plan.summary(nil)

	if got, want := summary.Cases, 12; got != want {
		t.Errorf("cases = %d, want %d", got, want)
	}
	if got, want := summary.AgentRuns, 10; got != want {
		t.Errorf("agent runs = %d, want %d", got, want)
	}
	if got, want := summary.Proposals, 10; got != want {
		t.Errorf("proposals = %d, want %d", got, want)
	}
	if got, want := summary.Approvals, 5; got != want {
		t.Errorf("approvals = %d, want %d", got, want)
	}
	if got, want := summary.Approved, 3; got != want {
		t.Errorf("approved decisions = %d, want %d", got, want)
	}
	if got, want := summary.Rejected, 2; got != want {
		t.Errorf("rejected decisions = %d, want %d", got, want)
	}
	if got, want := summary.Applied, 6; got != want {
		t.Errorf("applied proposals = %d, want %d", got, want)
	}
	if got, want := summary.ApplyFailed, 2; got != want {
		t.Errorf("apply_failed proposals = %d, want %d", got, want)
	}
	if got, want := summary.Escalations, 4; got != want {
		t.Errorf("escalations = %d, want %d", got, want)
	}
	if got, want := summary.EscalationKnown["policy"], 2; got != want {
		t.Errorf("policy escalations = %d, want %d", got, want)
	}
	if got, want := summary.EscalationKnown["low_confidence"], 1; got != want {
		t.Errorf("low_confidence escalations = %d, want %d", got, want)
	}
	if got, want := summary.EscalationKnown["unknown"], 1; got != want {
		t.Errorf("unknown escalations = %d, want %d", got, want)
	}

	// Decisions land on at least two UTC days.
	decisionDays := map[string]bool{}
	for _, approval := range plan.Approvals {
		decisionDays[approval.DecidedAt.Format("2006-01-02")] = true
	}
	if len(decisionDays) < 2 {
		t.Errorf("approval decision days = %d, want at least 2", len(decisionDays))
	}

	// Both agent-run models/prompts are used by the production rows so the
	// override/apply views join to real dimensions.
	prodCombos := map[string]bool{}
	for _, run := range plan.Runs {
		prodCombos[run.Model+"/"+run.Prompt] = true
	}
	for _, model := range plan.Models {
		for _, prompt := range plan.Prompts {
			if !prodCombos[model+"/"+prompt] {
				t.Errorf("no production agent run for %s/%s", model, prompt)
			}
		}
	}

	// One apply_failed audit is deliberately attributed to a different UTC day
	// than the proposal's created_at.
	proposalCreated := map[string]string{}
	for _, proposal := range plan.Proposals {
		proposalCreated[proposal.ID] = proposal.CreatedAt.Format("2006-01-02")
	}
	var crossDay bool
	for _, entry := range plan.Audits {
		if entry.Action != "proposal.transition" || entry.EntityType != "proposal" {
			continue
		}
		after, ok := entry.After.(map[string]any)
		if !ok || after["status"] != "apply_failed" {
			continue
		}
		if entry.Timestamp.Format("2006-01-02") != proposalCreated[entry.EntityID] {
			crossDay = true
		}
	}
	if !crossDay {
		t.Error("no apply_failed audit falls on a different day from its proposal created_at")
	}

	// Every quality row carries a deterministic demo identifier or marker.
	seenIDs := map[string]string{}
	checkID := func(kind, id string) {
		if id == "" {
			t.Errorf("%s has an empty id", kind)
			return
		}
		if previous, duplicate := seenIDs[id]; duplicate {
			t.Errorf("duplicate id %s for %s and %s", id, previous, kind)
			return
		}
		seenIDs[id] = kind
	}
	for _, c := range plan.Cases {
		checkID("case", c.ID)
	}
	for _, run := range plan.Runs {
		checkID("agent_run", run.ID)
		if !strings.HasPrefix(run.TraceID, qualityKeyPrefix) {
			t.Errorf("agent run %s trace_id = %q, want prefix %q", run.Key, run.TraceID, qualityKeyPrefix)
		}
	}
	for _, proposal := range plan.Proposals {
		checkID("proposal", proposal.ID)
		if !strings.HasPrefix(proposal.IdempotencyKey, qualityKeyPrefix) {
			t.Errorf("proposal %s idempotency_key = %q, want prefix %q",
				proposal.Key, proposal.IdempotencyKey, qualityKeyPrefix)
		}
	}
	for _, approval := range plan.Approvals {
		checkID("approval", approval.ID)
	}
	for _, run := range plan.EvalRuns {
		checkID("eval_run", run.ID)
		for _, result := range run.Cases {
			checkID("eval_result", result.ID)
			if !strings.HasPrefix(result.ScenarioID, qualityKeyPrefix) {
				t.Errorf("eval case %s scenario_id = %q, want prefix %q",
					result.Key, result.ScenarioID, qualityKeyPrefix)
			}
		}
	}
	for _, approver := range plan.Approvers {
		if !strings.HasPrefix(approver.APIKeyID, qualityKeyPrefix) {
			t.Errorf("approver %s api_key_id = %q, want prefix %q",
				approver.Key, approver.APIKeyID, qualityKeyPrefix)
		}
	}
}

func TestQualitySummaryWrite(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	plan := testQualityPlan(1, now)
	var b strings.Builder
	if err := plan.summary(nil).Write(&b); err != nil {
		t.Fatalf("write summary: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"M1/ops demo layer: already present",
		"M5-05 quality demo summary: seed=1 anchor=2026-11-25",
		"eval_runs=13 eval_case_results=64",
		"approvals=5 approved=3 rejected=2",
		"applied=6 apply_failed=2 cases=12 escalations=4",
		"escalation_reasons: policy=2 low_confidence=1 unknown=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00000000-0000") {
		t.Errorf("summary leaks a UUID:\n%s", out)
	}
}
