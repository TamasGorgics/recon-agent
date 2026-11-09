package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/eval/score"
)

func appendixTruth(scenarios ...string) map[string]dataset.ScenarioTruth {
	out := make(map[string]dataset.ScenarioTruth, len(scenarios))
	for _, scenario := range scenarios {
		out[scenario] = dataset.ScenarioTruth{
			ScenarioID:          scenario,
			Mode:                "mode_" + scenario,
			Difficulty:          "realistic",
			ExpectedResolutions: []string{"link"},
		}
	}
	return out
}

func appendixResult(scenario string, correct, falseFix, escalated bool, outcome string,
	detail map[string]any) score.CaseResult {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["outcome"] = outcome
	detail["actual_resolution"] = "link"
	return score.CaseResult{
		Split:      "dev",
		ScenarioID: scenario,
		Mode:       "mode_" + scenario,
		Correct:    correct,
		FalseFix:   falseFix,
		Escalated:  escalated,
		Detail:     detail,
	}
}

func TestAppendixOrderingFalseFixIncorrectOverEscalationInfra(t *testing.T) {
	results := []score.CaseResult{
		appendixResult("infra", false, false, false, "provider_error", nil),
		appendixResult("over", false, false, true, "escalated", nil),
		appendixResult("wrong", false, false, false, "proposal", nil),
		appendixResult("fix", false, true, false, "proposal", nil),
	}
	data := []runReport{{run: RunRow{ID: "run-1"}, results: results}}
	cases, warnings := buildAppendix(data, appendixTruth("infra", "over", "wrong", "fix"),
		RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	want := []string{"fix", "wrong", "over", "infra"}
	if len(cases) != len(want) {
		t.Fatalf("cases = %d, want %d", len(cases), len(want))
	}
	for index, scenario := range want {
		if cases[index].ScenarioID != scenario {
			t.Fatalf("cases[%d] = %s, want %s", index, cases[index].ScenarioID, scenario)
		}
	}
}

func TestAppendixTieBreakByModeAndScenario(t *testing.T) {
	first := appendixResult("b", false, true, false, "proposal", nil)
	first.Mode = "aaa"
	second := appendixResult("a", false, true, false, "proposal", nil)
	second.Mode = "aaa"
	third := appendixResult("a", false, true, false, "proposal", nil)
	third.Mode = "bbb"
	truth := map[string]dataset.ScenarioTruth{
		"a": {ScenarioID: "a", Mode: "bbb", Difficulty: "realistic"},
		"b": {ScenarioID: "b", Mode: "aaa", Difficulty: "realistic"},
	}
	// Both "a" results share a scenario ID; the map keeps the last. Use unique
	// IDs while keeping the tie-break order explicit.
	second.ScenarioID = "a1"
	third.ScenarioID = "a2"
	truth["a1"] = dataset.ScenarioTruth{ScenarioID: "a1", Mode: "aaa", Difficulty: "realistic"}
	truth["a2"] = dataset.ScenarioTruth{ScenarioID: "a2", Mode: "bbb", Difficulty: "realistic"}
	cases, _ := buildAppendix([]runReport{{
		run:     RunRow{ID: "run-1"},
		results: []score.CaseResult{third, first, second},
	}}, truth, RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	want := []string{"a1", "b", "a2"}
	for index, scenario := range want {
		if cases[index].ScenarioID != scenario {
			t.Fatalf("cases[%d] = %s, want %s", index, cases[index].ScenarioID, scenario)
		}
	}
}

func TestAppendixCap(t *testing.T) {
	results := make([]score.CaseResult, 0, 25)
	truth := map[string]dataset.ScenarioTruth{}
	for index := 0; index < 25; index++ {
		scenario := fmt.Sprintf("s%02d", index)
		results = append(results, appendixResult(scenario, false, false, false, "proposal", nil))
		truth[scenario] = dataset.ScenarioTruth{
			ScenarioID: scenario, Mode: "mode_" + scenario, Difficulty: "realistic",
		}
	}
	cases, _ := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: results,
	}}, truth, RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 20 {
		t.Fatalf("appendix length = %d, want 20", len(cases))
	}
}

func TestAppendixDedupesRepeatsAndUsesFirstFailingTranscript(t *testing.T) {
	transcript := map[string]any{
		"version":    1,
		"outcome":    "proposal",
		"proposal":   nil,
		"assistant":  "first failure",
		"tool_steps": []any{},
		"truncated":  false,
	}
	first := appendixResult("scenario", false, true, false, "proposal",
		map[string]any{"transcript": transcript})
	second := appendixResult("scenario", false, true, false, "proposal", nil)
	truth := appendixTruth("scenario")
	cases, _ := buildAppendix([]runReport{
		{run: RunRow{ID: "run-0", RepeatIndex: intPointer(0)}, results: []score.CaseResult{first}},
		{run: RunRow{ID: "run-1", RepeatIndex: intPointer(1)}, results: []score.CaseResult{second}},
	}, truth, RunRow{ID: "run-0"}, Options{AppendixLimit: 20})
	if len(cases) != 1 {
		t.Fatalf("cases = %d, want one deduplicated scenario", len(cases))
	}
	if cases[0].RepeatsFailed != 2 || cases[0].Repeats != 2 {
		t.Fatalf("repeats = %d/%d", cases[0].RepeatsFailed, cases[0].Repeats)
	}
	if cases[0].TranscriptStatus != transcriptStatusAvailable ||
		cases[0].Transcript == nil || cases[0].Transcript.Assistant != "first failure" {
		t.Fatalf("example transcript = %+v", cases[0].Transcript)
	}
	if !strings.Contains(cases[0].Locator, "run-0") {
		t.Fatalf("locator = %q", cases[0].Locator)
	}
}

func TestAppendixTranscriptFallbacks(t *testing.T) {
	missing := appendixResult("missing", false, false, false, "proposal", nil)
	unsupported := appendixResult("unsupported", false, false, false, "proposal",
		map[string]any{"transcript": map[string]any{"version": 9}})
	malformed := appendixResult("malformed", false, false, false, "proposal",
		map[string]any{"transcript": "not-a-snapshot"})
	truth := appendixTruth("missing", "unsupported", "malformed")
	cases, warnings := buildAppendix([]runReport{{
		run:     RunRow{ID: "run-1"},
		results: []score.CaseResult{missing, unsupported, malformed},
	}}, truth, RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 3 {
		t.Fatalf("cases = %d", len(cases))
	}
	statuses := map[string]string{}
	for _, entry := range cases {
		statuses[entry.ScenarioID] = entry.TranscriptStatus
	}
	if statuses["missing"] != transcriptStatusUnavailable {
		t.Fatalf("missing status = %s", statuses["missing"])
	}
	if statuses["unsupported"] != transcriptStatusUnsupported {
		t.Fatalf("unsupported status = %s", statuses["unsupported"])
	}
	if statuses["malformed"] != transcriptStatusUnavailable {
		t.Fatalf("malformed status = %s", statuses["malformed"])
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestAppendixTranscriptRedactionAndNoRawData(t *testing.T) {
	seededAssistant := "memo oc_sk_seeded Bearer seeded-token " +
		"postgres://recon:secret@db.example/recon ses_0123456789abcdef " +
		strings.Repeat("a", 300)
	transcript := map[string]any{
		"version":   1,
		"outcome":   "proposal",
		"proposal":  map[string]any{"id": "proposal-1", "type": "link"},
		"assistant": seededAssistant,
		"tool_steps": []any{
			map[string]any{"seq": 1, "tool": "get_case", "refs": []any{"case:1"},
				"error_code": "", "truncated": false},
		},
		"truncated": false,
		"args":      map[string]any{"secret": "args-secret"},
		"result":    map[string]any{"secret": "result-secret"},
		"memo":      "memo-secret",
		"api_key":   "oc_sk_extra",
		"dsn":       "postgres://user:pass@db/x",
	}
	result := appendixResult("scenario", false, true, false, "proposal",
		map[string]any{"transcript": transcript})
	result.Confidence = decimalPointer("0.5")
	cases, _ := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: []score.CaseResult{result},
	}}, appendixTruth("scenario"), RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 1 {
		t.Fatalf("cases = %d", len(cases))
	}
	snapshot := cases[0].Transcript
	if snapshot == nil || snapshot.Assistant == "" {
		t.Fatal("transcript missing")
	}
	if len([]rune(snapshot.Assistant)) != transcriptRuneCap {
		t.Fatalf("assistant runes = %d, want %d",
			len([]rune(snapshot.Assistant)), transcriptRuneCap)
	}
	encoded, err := json.Marshal(cases[0])
	if err != nil {
		t.Fatalf("marshal appendix case: %v", err)
	}
	rendered := string(encoded)
	for _, forbidden := range []string{
		"args-secret", "result-secret", "memo-secret", "oc_sk_extra",
		"oc_sk_seeded", "seeded-token", "secret@db.example", "ses_0123456789abcdef",
		`"args"`, `"result"`, `"memo"`, `"api_key"`, `"dsn"`,
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered appendix leaks %q: %s", forbidden, rendered)
		}
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %s", rendered)
	}
}

func TestAppendixRedactsEscapedJSONAuthorization(t *testing.T) {
	oneLevel := `{"error":"{\"authorization\":\"Basic s3cr3t-one\"}"}`
	twoLevel := `{"message":"{\\\"authorization\\\":\\\"Basic s3cr3t-two\\\"}"}`
	transcript := map[string]any{
		"version":   1,
		"outcome":   "proposal",
		"proposal":  map[string]any{"id": "proposal-1", "type": "link"},
		"assistant": "escaped memo " + oneLevel + "\nnext assistant line",
		"tool_steps": []any{
			map[string]any{"seq": 1, "tool": "get_case", "refs": []any{},
				"error_code": twoLevel + "\nnext error line", "truncated": false},
		},
		"truncated": false,
	}
	result := appendixResult("scenario", false, true, false, "proposal",
		map[string]any{"transcript": transcript})

	cases, _ := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: []score.CaseResult{result},
	}}, appendixTruth("scenario"), RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 1 || cases[0].Transcript == nil {
		t.Fatalf("cases = %+v", cases)
	}
	snapshot := cases[0].Transcript
	if !strings.Contains(snapshot.Assistant, "\nnext assistant line") {
		t.Fatalf("assistant following line was consumed: %q", snapshot.Assistant)
	}
	if !strings.Contains(snapshot.ToolSteps[0].ErrorCode, "\nnext error line") {
		t.Fatalf("error following line was consumed: %q", snapshot.ToolSteps[0].ErrorCode)
	}
	encoded, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal appendix: %v", err)
	}
	rendered := string(encoded)
	for _, secret := range []string{"Basic", "s3cr3t-one", "s3cr3t-two"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("report leaked %q: %s", secret, rendered)
		}
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Fatalf("report did not record redaction: %s", rendered)
	}
	// The snapshot itself remains a valid JSON document.
	roundTrip, err := json.Marshal(snapshot)
	if err != nil || !json.Valid(roundTrip) {
		t.Fatalf("snapshot is not valid JSON: %v %s", err, roundTrip)
	}
	for _, value := range []string{snapshot.Assistant, snapshot.ToolSteps[0].ErrorCode} {
		if again := redactTranscriptText(value); again != value {
			t.Fatalf("report redaction is not idempotent: %q -> %q", value, again)
		}
	}
}

func TestAppendixDropsCredentialRefsAndKeepsCanonicalOnes(t *testing.T) {
	const (
		caseRef = "case:123e4567-e89b-42d3-a456-426614174000"
		extRef  = "ext:psp_a:rec-2"
	)
	transcript := map[string]any{
		"version":   1,
		"outcome":   "proposal",
		"proposal":  map[string]any{"id": "proposal-1", "type": "link"},
		"assistant": "safe",
		"tool_steps": []any{map[string]any{
			"seq": 1, "tool": "get_case",
			"refs": []any{
				extRef,
				caseRef,
				"ext:psp_a:mysql://recon:s3cr3t@db.example/recon",
				"ext:psp_a:https://recon:s3cr3t@db.example/recon",
				"ext:psp_a:token=abcdef123456",
				"ext:psp_a:api_key=abcdef123456",
				"ext:psp_a:sk_live_abcdef123456",
			},
			"error_code": "", "truncated": false,
		}},
		"truncated": false,
	}
	result := appendixResult("scenario", false, true, false, "proposal",
		map[string]any{"transcript": transcript})

	cases, _ := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: []score.CaseResult{result},
	}}, appendixTruth("scenario"), RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 1 || cases[0].Transcript == nil {
		t.Fatalf("cases = %+v", cases)
	}
	snapshot := cases[0].Transcript
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, secret := range []string{
		"s3cr3t", "token=abcdef123456", "api_key=abcdef123456", "sk_live_abcdef123456",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report leaked %q: %s", secret, encoded)
		}
	}
	if got, want := strings.Join(snapshot.ToolSteps[0].Refs, ","), caseRef+","+extRef; got != want {
		t.Fatalf("refs = %q, want only canonical safe refs %q", got, want)
	}
}

func TestAppendixSanitizesForeignV1Snapshot(t *testing.T) {
	const (
		caseRef = "case:123e4567-e89b-42d3-a456-426614174000"
		extRef  = "ext:psp_a:rec-2"
	)
	transcript := map[string]any{
		"version":   1,
		"outcome":   "proposal",
		"proposal":  map[string]any{"id": "proposal-1", "type": "link"},
		"assistant": "debug password=hunter2 dsn postgres://user:pass@db.example/recon",
		"tool_steps": []any{
			map[string]any{
				"seq":  1,
				"tool": "get_case",
				"refs": []any{
					caseRef,
					extRef,
					"ext:psp_a:postgres://user:pass@db.example/recon",
					"ext:psp_a:oc_sk_synthetic_secret",
					"ext:psp_a:ses_abc123",
					"ext:psp_a:password=hunter2",
					"ext:psp_a:rec\x00bad",
					"not-a-ref",
				},
				"error_code": "Authorization: Basic dXNlcjpwYXNzd29yZA==",
				"truncated":  false,
			},
		},
		"truncated": false,
	}
	result := appendixResult("scenario", false, true, false, "proposal",
		map[string]any{"transcript": transcript})
	result.Detail["error"] = "Authorization: Token abc123"

	cases, _ := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: []score.CaseResult{result},
	}}, appendixTruth("scenario"), RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 1 {
		t.Fatalf("cases = %d", len(cases))
	}
	snapshot := cases[0].Transcript
	if snapshot == nil {
		t.Fatal("transcript missing")
	}
	if got, want := strings.Join(snapshot.ToolSteps[0].Refs, ","), caseRef+","+extRef; got != want {
		t.Fatalf("refs = %q, want only canonical safe refs %q", got, want)
	}
	if strings.Contains(snapshot.ToolSteps[0].ErrorCode, "dXNlcjpwYXNzd29yZA==") {
		t.Fatalf("step error code leaked: %q", snapshot.ToolSteps[0].ErrorCode)
	}
	if strings.Contains(cases[0].ErrorCode, "abc123") {
		t.Fatalf("case error code leaked: %q", cases[0].ErrorCode)
	}

	markdown, err := RenderMarkdown(Report{FailureAppendix: cases})
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	encoded, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal appendix: %v", err)
	}
	rendered := string(markdown) + string(encoded)
	for _, secret := range []string{
		"hunter2", "user:pass@db.example", "oc_sk_synthetic_secret",
		"ses_abc123", "dXNlcjpwYXNzd29yZA==", "abc123", "not-a-ref",
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("report leaked %q: %s", secret, rendered)
		}
	}
	for _, want := range []string{caseRef, extRef, "[REDACTED]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("report missing %q: %s", want, rendered)
		}
	}
}

func TestAppendixUnavailableDoesNotFail(t *testing.T) {
	result := appendixResult("scenario", false, false, false, "proposal", nil)
	cases, warnings := buildAppendix([]runReport{{
		run: RunRow{ID: "run-1"}, results: []score.CaseResult{result},
	}}, appendixTruth("scenario"), RunRow{ID: "run-1"}, Options{AppendixLimit: 20})
	if len(cases) != 1 || cases[0].Transcript != nil {
		t.Fatalf("cases = %+v", cases)
	}
	if cases[0].Locator == "" {
		t.Fatal("missing locator")
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func decimalPointer(value string) *decimal.Decimal {
	parsed := decimal.RequireFromString(value)
	return &parsed
}
