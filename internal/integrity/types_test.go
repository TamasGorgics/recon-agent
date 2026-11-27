// covers: FIN-52
package integrity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCheckOrder(t *testing.T) {
	want := []CheckName{
		"ledger_balance",
		"value_conservation",
		"applied_proposal_trail",
		"audit_chain",
		"tieout",
	}
	if len(CheckOrder) != len(want) {
		t.Fatalf("CheckOrder has %d entries, want %d", len(CheckOrder), len(want))
	}
	for i := range want {
		if CheckOrder[i] != want[i] {
			t.Fatalf("CheckOrder[%d] = %q, want %q", i, CheckOrder[i], want[i])
		}
	}
}

func passedCheck(name CheckName, checked, skipped int64) CheckResult {
	return finalizeCheckResult(name, checked, skipped, nil)
}

func TestReportJSONShape(t *testing.T) {
	started := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	finished := started.Add(3412 * time.Millisecond)
	checks := make([]CheckResult, 0, len(CheckOrder))
	for _, name := range CheckOrder {
		checks = append(checks, passedCheck(name, 7, 0))
	}
	report := deriveReport(NewDate(started), started, finished, checks)
	report.RunID = "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b"
	if err := report.validate(); err != nil {
		t.Fatalf("validate report: %v", err)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	for _, field := range []string{
		"run_id", "run_date", "started_at", "finished_at", "status",
		"check_count", "failure_count", "checks",
	} {
		if _, ok := document[field]; !ok {
			t.Fatalf("report JSON is missing %q: %s", field, encoded)
		}
	}

	var runDate string
	if err := json.Unmarshal(document["run_date"], &runDate); err != nil {
		t.Fatalf("run_date is not a string: %v", err)
	}
	if runDate != "2026-11-26" {
		t.Fatalf("run_date = %q, want 2026-11-26", runDate)
	}
	var startedText, finishedText string
	if err := json.Unmarshal(document["started_at"], &startedText); err != nil {
		t.Fatalf("started_at is not a string: %v", err)
	}
	if err := json.Unmarshal(document["finished_at"], &finishedText); err != nil {
		t.Fatalf("finished_at is not a string: %v", err)
	}
	for _, value := range []string{startedText, finishedText} {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatalf("timestamp %q is not RFC3339: %v", value, err)
		}
		if parsed.Location() != time.UTC {
			t.Fatalf("timestamp %q is not UTC", value)
		}
	}

	var checkCount int
	if err := json.Unmarshal(document["check_count"], &checkCount); err != nil {
		t.Fatalf("check_count: %v", err)
	}
	if checkCount != len(CheckOrder) {
		t.Fatalf("check_count = %d, want %d", checkCount, len(CheckOrder))
	}

	var checksDocument []map[string]json.RawMessage
	if err := json.Unmarshal(document["checks"], &checksDocument); err != nil {
		t.Fatalf("checks: %v", err)
	}
	if len(checksDocument) != len(CheckOrder) {
		t.Fatalf("checks length = %d, want %d", len(checksDocument), len(CheckOrder))
	}
	for i, check := range checksDocument {
		for _, field := range []string{"name", "status", "checked", "skipped", "problem_count", "truncated", "problems"} {
			if _, ok := check[field]; !ok {
				t.Fatalf("check %d is missing %q", i, field)
			}
		}
		if string(check["problems"]) != "[]" {
			t.Fatalf("passed check %d problems = %s, want []", i, check["problems"])
		}
		var name CheckName
		if err := json.Unmarshal(check["name"], &name); err != nil {
			t.Fatalf("check %d name: %v", i, err)
		}
		if name != CheckOrder[i] {
			t.Fatalf("check %d name = %q, want %q", i, name, CheckOrder[i])
		}
	}
}

func TestFailureCountCountsChecksNotProblems(t *testing.T) {
	checks := make([]CheckResult, 0, len(CheckOrder))
	for _, name := range CheckOrder {
		checks = append(checks, passedCheck(name, 1, 0))
	}
	checks[1] = finalizeCheckResult(CheckValueConservation, 2, 0, []Problem{
		{Code: CodeValueConservationMismatch, Entity: "source_currency", ID: "psp_a:USD", Detail: "a"},
		{Code: CodeValueConservationMismatch, Entity: "source_currency", ID: "psp_a:EUR", Detail: "b"},
	})
	checks[4] = finalizeCheckResult(CheckTieout, 1, 0, []Problem{
		{Code: CodeTieoutUnexplained, Entity: "scope", ID: "scope-1", Detail: "c"},
	})

	started := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	report := deriveReport(NewDate(started), started, started.Add(time.Second), checks)
	if report.FailureCount != 2 {
		t.Fatalf("failure_count = %d, want 2 (failed checks, not problems)", report.FailureCount)
	}
	if report.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", report.Status)
	}
	if report.Checks[1].ProblemCount != 2 {
		t.Fatalf("value problem_count = %d, want 2", report.Checks[1].ProblemCount)
	}
	if err := report.validate(); err != nil {
		t.Fatalf("validate failed report: %v", err)
	}
}

func TestProblemCapAndSort(t *testing.T) {
	problems := make([]Problem, 0, 150)
	for i := 150; i > 0; i-- {
		problems = append(problems, Problem{
			Code:   "code_b",
			Entity: "entity",
			ID:     strings.Repeat("z", 3),
			Detail: "detail",
		})
	}
	problems = append(problems, Problem{Code: "code_a", Entity: "entity", ID: "id", Detail: "first"})

	result := finalizeCheckResult(CheckLedgerBalance, 3, 0, problems)
	if result.ProblemCount != 151 {
		t.Fatalf("problem_count = %d, want 151 (pre-cap)", result.ProblemCount)
	}
	if len(result.Problems) != MaxPersistedProblems {
		t.Fatalf("problems length = %d, want %d", len(result.Problems), MaxPersistedProblems)
	}
	if !result.Truncated {
		t.Fatal("truncated = false, want true")
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.Problems[0].Code != "code_a" {
		t.Fatalf("first retained problem code = %q, want code_a (sorted before capping)", result.Problems[0].Code)
	}
	for i := 1; i < len(result.Problems); i++ {
		previous, current := result.Problems[i-1], result.Problems[i]
		if previous.Code > current.Code ||
			(previous.Code == current.Code && previous.Entity > current.Entity) ||
			(previous.Code == current.Code && previous.Entity == current.Entity && previous.ID > current.ID) ||
			(previous.Code == current.Code && previous.Entity == current.Entity && previous.ID == current.ID && previous.Detail > current.Detail) {
			t.Fatalf("problems are not sorted at %d: %+v then %+v", i, previous, current)
		}
	}
}

func TestBoundedDetailRedaction(t *testing.T) {
	if got := boundedDetail("first\nsecond\tthird\r\nfourth"); got != "first second third fourth" {
		t.Fatalf("boundedDetail collapsed value = %q", got)
	}
	long := strings.Repeat("é", maxDetailLen+50)
	got := boundedDetail(long)
	if count := len([]rune(got)); count != maxDetailLen {
		t.Fatalf("boundedDetail length = %d runes, want %d", count, maxDetailLen)
	}
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("boundedDetail retained a control character: %q", got)
	}
	if boundedDetail("") != "" {
		t.Fatalf("boundedDetail of empty string = %q", boundedDetail(""))
	}
}

func TestDateJSON(t *testing.T) {
	date := NewDate(time.Date(2026, 11, 26, 23, 59, 59, 0, time.FixedZone("offset", 3600)))
	encoded, err := json.Marshal(date)
	if err != nil {
		t.Fatalf("marshal date: %v", err)
	}
	if string(encoded) != `"2026-11-26"` {
		t.Fatalf("date JSON = %s, want \"2026-11-26\"", encoded)
	}
	var decoded Date
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal date: %v", err)
	}
	if decoded.String() != "2026-11-26" {
		t.Fatalf("decoded date = %q", decoded.String())
	}
	if _, err := ParseDate("2026-13-01"); err == nil {
		t.Fatal("ParseDate accepted an invalid month")
	}
}

func TestReportValidateRejectsShapeViolations(t *testing.T) {
	started := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	checks := make([]CheckResult, 0, len(CheckOrder))
	for _, name := range CheckOrder {
		checks = append(checks, passedCheck(name, 1, 0))
	}

	short := deriveReport(NewDate(started), started, started, checks[:4])
	if err := short.validate(); err == nil {
		t.Fatal("validate accepted a four-check report")
	}

	wrongOrder := append([]CheckResult{}, checks...)
	wrongOrder[0], wrongOrder[1] = wrongOrder[1], wrongOrder[0]
	reordered := deriveReport(NewDate(started), started, started, wrongOrder)
	if err := reordered.validate(); err == nil {
		t.Fatal("validate accepted a reordered report")
	}

	backwards := deriveReport(NewDate(started), started, started.Add(-time.Second), checks)
	if err := backwards.validate(); err != nil {
		t.Fatalf("deriveReport should clamp finished_at, got %v", err)
	}
	if backwards.FinishedAt.Before(backwards.StartedAt) {
		t.Fatal("finished_at is before started_at after clamping")
	}
}
