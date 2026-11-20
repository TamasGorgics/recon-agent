package approver

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// TestNoTruthImportsOrAccess is the package-boundary proof: the non-test
// package sources neither import an answer source nor name one in SQL.
func TestNoTruthImportsOrAccess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	forbiddenImportSuffixes := []string{
		"/eval/dataset",
		"/eval/score",
		"/eval/truth",
		"/eval/runner",
		"/eval/gate",
		"/eval/report",
	}
	forbiddenText := []string{
		"FROM eval_truth",
		"eval_truth.scenarios",
		"eval_truth.scenario_cases",
		"eval_truth.sandbox_meta",
		"expected_resolutions",
		"primary_resolution",
		"break_signature",
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			for _, suffix := range forbiddenImportSuffixes {
				if strings.HasSuffix(path, suffix) || strings.Contains(path, suffix+"/") {
					t.Errorf("%s imports forbidden answer-source package %q", name, path)
				}
			}
			if path == "net" || strings.HasPrefix(path, "net/") || path == "os/exec" {
				t.Errorf("%s imports network/process package %q", name, path)
			}
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, text := range forbiddenText {
			if strings.Contains(string(source), text) {
				t.Errorf("%s contains forbidden answer-source text %q", name, text)
			}
		}
	}
}

// hiddenTruth stands in for a ground-truth fixture. It is intentionally never
// passed to Act: the point of the negative test is that a truth-only change
// (the truth says link while the observable facts support adjustment) cannot
// change the decision.
type hiddenTruth struct {
	primaryResolution string
	expectedTier      string
	breakSignature    string
}

func TestTruthOnlyMismatchDoesNotInfluenceDecision(t *testing.T) {
	truths := []hiddenTruth{
		{primaryResolution: "link", expectedTier: "T1", breakSignature: "aaaa"},
		{primaryResolution: "adjustment", expectedTier: "T2", breakSignature: "bbbb"},
	}

	var outcomes []Outcome
	for _, truth := range truths {
		caseFacts, proposal, records := adjustmentFacts()
		decider := &fakeDecider{}
		a := newTestApprover(t, decider, records)
		outcome, err := a.Act(t.Context(), pendingRequest(policy.TierT1), caseFacts, proposal)
		if err != nil {
			t.Fatalf("Act with hidden truth %+v: %v", truth, err)
		}
		outcomes = append(outcomes, outcome)
	}

	if outcomes[0] != outcomes[1] {
		t.Fatalf("outcomes differ after a truth-only change: %+v vs %+v", outcomes[0], outcomes[1])
	}
	if outcomes[0].Action != ActionApprove || outcomes[0].Rule != RuleExactMatch {
		t.Fatalf("outcome = %s/%s, want the observable exact match to approve",
			outcomes[0].Action, outcomes[0].Rule)
	}
}

// TestObservableExactMatchNeedsNoTruthSource exercises the same matcher with
// no truth fixture at all. The unit test binary has no database or eval_truth
// access, so a passing decision is itself evidence that the observable path
// is self-contained.
func TestObservableExactMatchNeedsNoTruthSource(t *testing.T) {
	caseFacts, proposal, records := duplicateFacts()
	decider := &fakeDecider{}
	a := newTestApprover(t, decider, records)
	outcome, err := a.Act(t.Context(), pendingRequest(policy.TierT2), caseFacts, proposal)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if outcome.Action != ActionApprove || outcome.Rule != RuleExactMatch {
		t.Fatalf("outcome = %s/%s, want approve/exact_match", outcome.Action, outcome.Rule)
	}
}
