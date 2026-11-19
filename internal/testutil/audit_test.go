// covers: FIN-30, FIN-31, FIN-32, FIN-33, FIN-34

package testutil

import (
	"fmt"
	"strings"
	"testing"
)

// recordingReporter captures completeness failures so the public *testing.T
// contract does not need a subprocess to prove the all-in-one-failure behavior.
type recordingReporter struct {
	helperCalls int
	failures    []string
}

func (r *recordingReporter) Helper() { r.helperCalls++ }

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// onlyFailure requires exactly one recorded failure and returns it.
func (r *recordingReporter) onlyFailure(t *testing.T) string {
	t.Helper()
	if len(r.failures) != 1 {
		t.Fatalf("recorded %d failures, want exactly 1: %v", len(r.failures), r.failures)
	}
	return r.failures[0]
}

func tripleSet(items ...Expectation) map[Expectation]struct{} {
	set := make(map[Expectation]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

func auditTriple(action, entityType, entityID string) Expectation {
	return Expectation{Action: action, EntityType: entityType, EntityID: entityID}
}

// mustContainInOrder asserts that parts appear in msg in the given order.
func mustContainInOrder(t *testing.T, msg string, parts ...string) {
	t.Helper()
	searchFrom := 0
	for _, part := range parts {
		idx := strings.Index(msg[searchFrom:], part)
		if idx < 0 {
			t.Fatalf("failure message does not contain %q after offset %d:\n%s", part, searchFrom, msg)
		}
		searchFrom += idx + len(part)
	}
}

func TestCheckCompletenessPassesWhenAllExpectedPresent(t *testing.T) {
	a := auditTriple("case.create", "case", "case-a")
	b := auditTriple("break.create", "break", "break-b")
	extra := auditTriple("ledger.post", "ledger_txn", "txn-1")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{a, b, a}, tripleSet(a, b, extra), nil, false)
	if len(rec.failures) != 0 {
		t.Fatalf("failures = %v, want none when every expected triple is present", rec.failures)
	}
	if rec.helperCalls == 0 {
		t.Error("checkAuditCompleteness did not call Helper")
	}
}

func TestCheckCompletenessReportsSingleMissing(t *testing.T) {
	present := auditTriple("case.create", "case", "case-a")
	missing := auditTriple("case.transition", "case", "case-a")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{present, missing}, tripleSet(present), nil, false)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, "1 of 2 expected audit triples missing") {
		t.Errorf("failure omits the missing count:\n%s", msg)
	}
	if !strings.Contains(msg, missing.String()) {
		t.Errorf("failure omits missing triple %s:\n%s", missing.String(), msg)
	}
}

func TestCheckCompletenessReportsEveryMissingInOneOrderedFailure(t *testing.T) {
	present := auditTriple("ledger.post", "ledger_txn", "txn-1")
	// Deliberately unsorted input; the message is sorted by entity type, entity
	// ID, then action.
	expected := []Expectation{
		auditTriple("case.transition", "case", "case-b"),
		auditTriple("break.create", "break", "break-z"),
		auditTriple("case.create", "case", "case-b"),
		auditTriple("case.create", "case", "case-a"),
		present,
	}

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, expected, tripleSet(present), nil, false)
	msg := rec.onlyFailure(t)

	if !strings.Contains(msg, "4 of 5 expected audit triples missing") {
		t.Errorf("failure omits the missing count:\n%s", msg)
	}
	mustContainInOrder(t, msg,
		auditTriple("break.create", "break", "break-z").String(),
		auditTriple("case.create", "case", "case-a").String(),
		auditTriple("case.create", "case", "case-b").String(),
		auditTriple("case.transition", "case", "case-b").String(),
	)
}

func TestCheckCompletenessDeduplicatesExpectedAndMissing(t *testing.T) {
	missing := auditTriple("case.transition", "case", "case-a")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{missing, missing, missing}, tripleSet(), nil, false)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, "1 of 1 expected audit triples missing") {
		t.Errorf("duplicate expectations were not deduplicated:\n%s", msg)
	}
	if got := strings.Count(msg, missing.String()); got != 1 {
		t.Errorf("missing triple listed %d times, want once:\n%s", got, msg)
	}
}

func TestCheckCompletenessEmptyExpectedPasses(t *testing.T) {
	extra := auditTriple("case.create", "case", "case-a")
	rec := &recordingReporter{}
	checkAuditCompleteness(rec, nil, tripleSet(extra), nil, false)
	if len(rec.failures) != 0 {
		t.Fatalf("failures = %v, want none for empty non-exact expectations", rec.failures)
	}
}

func TestCheckCompletenessFailureIsDeterministic(t *testing.T) {
	present := auditTriple("ledger.post", "ledger_txn", "txn-1")
	expected := []Expectation{
		auditTriple("case.transition", "case", "case-b"),
		auditTriple("break.create", "break", "break-z"),
		auditTriple("case.create", "case", "case-b"),
		auditTriple("case.create", "case", "case-a"),
		present,
	}

	var first string
	for i := 0; i < 25; i++ {
		rec := &recordingReporter{}
		checkAuditCompleteness(rec, expected, tripleSet(present), nil, false)
		msg := rec.onlyFailure(t)
		if i == 0 {
			first = msg
			continue
		}
		if msg != first {
			t.Fatalf("failure message changed on run %d:\nfirst:\n%s\nnow:\n%s", i, first, msg)
		}
	}
}

func TestCheckExactPassesWhenActualMatchesExpected(t *testing.T) {
	a := auditTriple("case.create", "case", "case-a")
	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{a, a}, tripleSet(a), map[string]struct{}{"case-a": {}}, true)
	if len(rec.failures) != 0 {
		t.Fatalf("failures = %v, want none for an exact match", rec.failures)
	}
}

func TestCheckExactRejectsUnexpectedInScope(t *testing.T) {
	expected := auditTriple("case.create", "case", "case-a")
	unexpected := auditTriple("case.transition", "case", "case-b")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{expected},
		tripleSet(expected, unexpected),
		map[string]struct{}{"case-a": {}, "case-b": {}}, true)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, "0 of 1 expected audit triples missing") {
		t.Errorf("unexpected-only failure should still report the missing count:\n%s", msg)
	}
	if !strings.Contains(msg, "unexpected in scope:") || !strings.Contains(msg, unexpected.String()) {
		t.Errorf("failure omits the unexpected in-scope triple %s:\n%s", unexpected.String(), msg)
	}
	if strings.Contains(msg, "\nmissing:") {
		t.Errorf("unexpected-only failure has an empty missing section:\n%s", msg)
	}
}

func TestCheckExactIgnoresUnexpectedOutsideScope(t *testing.T) {
	expected := auditTriple("case.create", "case", "case-a")
	outside := auditTriple("case.transition", "case", "case-other")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{expected},
		tripleSet(expected, outside),
		map[string]struct{}{"case-a": {}}, true)
	if len(rec.failures) != 0 {
		t.Fatalf("failures = %v, want none: an outside-scope triple must be ignored", rec.failures)
	}
}

func TestCheckExactGlobalScopeRejectsUnexpected(t *testing.T) {
	expected := auditTriple("case.create", "case", "case-a")
	unexpected := auditTriple("case.transition", "case", "case-other")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{expected}, tripleSet(expected, unexpected), nil, true)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, unexpected.String()) {
		t.Errorf("global exact mode did not reject %s:\n%s", unexpected.String(), msg)
	}
}

func TestCheckExactReportsMissingAndUnexpectedTogether(t *testing.T) {
	present := auditTriple("case.create", "case", "case-a")
	unexpected := auditTriple("case.transition", "case", "case-b")
	missing := auditTriple("case.transition", "case", "case-d")

	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{present, missing}, tripleSet(present, unexpected), nil, true)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, "1 of 2 expected audit triples missing, 1 unexpected audit triples in scope") {
		t.Errorf("failure omits the combined counts:\n%s", msg)
	}
	if !strings.Contains(msg, "\nmissing:") || !strings.Contains(msg, missing.String()) {
		t.Errorf("failure omits the missing triple %s:\n%s", missing.String(), msg)
	}
	if !strings.Contains(msg, "\nunexpected in scope:") || !strings.Contains(msg, unexpected.String()) {
		t.Errorf("failure omits the unexpected triple %s:\n%s", unexpected.String(), msg)
	}
}

func TestCheckExactMissingIsAlwaysGlobal(t *testing.T) {
	// The missing expected triple's entity is outside the scope; scope narrows
	// only the unexpected set, so the miss must still fail.
	missing := auditTriple("case.transition", "case", "case-outside")
	rec := &recordingReporter{}
	checkAuditCompleteness(rec, []Expectation{missing}, tripleSet(),
		map[string]struct{}{"case-inside": {}}, true)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, "1 of 1 expected audit triples missing") || !strings.Contains(msg, missing.String()) {
		t.Errorf("scope incorrectly narrowed the missing set:\n%s", msg)
	}
}

func TestCheckExactEmptyExpectedAndActualPasses(t *testing.T) {
	rec := &recordingReporter{}
	checkAuditCompleteness(rec, nil, tripleSet(), nil, true)
	if len(rec.failures) != 0 {
		t.Fatalf("failures = %v, want none for empty expected and actual sets", rec.failures)
	}
}

func TestCheckExactEmptyExpectedGlobalRejectsPresent(t *testing.T) {
	present := auditTriple("case.create", "case", "case-a")
	rec := &recordingReporter{}
	checkAuditCompleteness(rec, nil, tripleSet(present), nil, true)
	msg := rec.onlyFailure(t)
	if !strings.Contains(msg, present.String()) {
		t.Errorf("global exact mode accepted an unexpected triple:\n%s", msg)
	}
}

func TestSortExpectationsOrdersByEntityTypeEntityIDAction(t *testing.T) {
	got := []Expectation{
		auditTriple("case.transition", "case", "case-b"),
		auditTriple("case.create", "case", "case-b"),
		auditTriple("break.create", "break", "break-z"),
		auditTriple("case.create", "case", "case-a"),
	}
	sortExpectations(got)
	want := []Expectation{
		auditTriple("break.create", "break", "break-z"),
		auditTriple("case.create", "case", "case-a"),
		auditTriple("case.create", "case", "case-b"),
		auditTriple("case.transition", "case", "case-b"),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
