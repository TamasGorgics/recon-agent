package agent_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

func newTestTraceProvider(t *testing.T) (*telemetry.Provider, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider, err := telemetry.New(context.Background(),
		telemetry.Config{ServiceName: "recon-agent"},
		telemetry.WithExporter(exporter),
		telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatalf("provider.Shutdown: %v", err)
		}
	})
	return provider, exporter
}

func spansNamed(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var matched tracetest.SpanStubs
	for _, span := range spans {
		if span.Name == name {
			matched = append(matched, span)
		}
	}
	return matched
}

func singleSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	matched := spansNamed(spans, name)
	if len(matched) != 1 {
		t.Fatalf("%q spans = %d, want 1", name, len(matched))
	}
	return matched[0]
}

func spanAttribute(t *testing.T, span tracetest.SpanStub, key string) attribute.Value {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value
		}
	}
	t.Fatalf("span %q has no attribute %q", span.Name, key)
	return attribute.Value{}
}

func requireStringAttribute(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	got := spanAttribute(t, span, key)
	if got.AsString() != want {
		t.Fatalf("span %q %s = %q, want %q", span.Name, key, got.AsString(), want)
	}
}

func requireIntAttribute(t *testing.T, span tracetest.SpanStub, key string, want int64) {
	t.Helper()
	got := spanAttribute(t, span, key)
	if got.AsInt64() != want {
		t.Fatalf("span %q %s = %d, want %d", span.Name, key, got.AsInt64(), want)
	}
}

func requireHashAttribute(t *testing.T, span tracetest.SpanStub, key string) {
	t.Helper()
	value := spanAttribute(t, span, key).AsString()
	if len(value) != 64 || strings.Trim(value, "0123456789abcdef") != "" {
		t.Fatalf("span %q %s = %q, want a lowercase 64-hex hash", span.Name, key, value)
	}
}

func requireNoAttribute(t *testing.T, span tracetest.SpanStub, key string) {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			t.Fatalf("span %q unexpectedly has attribute %q", span.Name, key)
		}
	}
}

func TestRunTracingSpanTreeAndAttributes(t *testing.T) {
	t.Parallel()

	provider, exporter := newTestTraceProvider(t)
	h := newHarness(t, runnerOptions{
		telemetry: provider,
		steps: []llm.ScriptedStep{
			{Response: toolResponse("call_1", "read_thing", `{}`)},
			{Response: toolResponse("call_2", "propose_adjustment", `{}`)},
		},
	})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeProposal {
		t.Fatalf("outcome = %s, want proposal", result.Outcome)
	}

	spans := exporter.GetSpans()
	if len(spans) != 6 {
		t.Fatalf("recorded %d spans, want 1 case.run + 2 llm.call + 3 tool.call", len(spans))
	}
	root := singleSpan(t, spans, "case.run")
	if root.Parent.IsValid() {
		t.Fatalf("case.run parent = %s, want a root span", root.Parent.SpanID())
	}
	if !root.SpanContext.IsValid() {
		t.Fatal("case.run span context is invalid")
	}
	for _, span := range spans {
		if span.Name == "case.run" {
			continue
		}
		if span.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Fatalf("%s parent = %s, want %s", span.Name, span.Parent.SpanID(), root.SpanContext.SpanID())
		}
		if span.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Fatalf("%s trace = %s, want %s", span.Name, span.SpanContext.TraceID(), root.SpanContext.TraceID())
		}
	}

	// case.run: start attributes plus the deferred terminal attributes.
	requireStringAttribute(t, root, telemetry.AttrCaseID, testCaseID)
	requireStringAttribute(t, root, telemetry.AttrRunID, testRunID)
	requireStringAttribute(t, root, telemetry.AttrModel, testModel)
	requireStringAttribute(t, root, telemetry.AttrPromptVersion, "v1")
	requireIntAttribute(t, root, telemetry.AttrCaseVersion, 3)
	requireStringAttribute(t, root, telemetry.AttrRunOutcome, string(agent.OutcomeProposal))
	requireStringAttribute(t, root, telemetry.AttrProposalID, "33333333-3333-4333-8333-333333333333")
	requireIntAttribute(t, root, telemetry.AttrTokensInput, 20)
	requireIntAttribute(t, root, telemetry.AttrTokensOutput, 10)
	requireIntAttribute(t, root, telemetry.AttrCostMicroUSD, 20000)
	requireStringAttribute(t, root, telemetry.AttrCostCurrency, "USD")
	if root.Status.Code != codes.Ok {
		t.Fatalf("case.run status = %s, want Ok", root.Status.Code)
	}

	llmSpans := spansNamed(spans, "llm.call")
	if len(llmSpans) != 2 {
		t.Fatalf("llm.call spans = %d, want 2", len(llmSpans))
	}
	slices.SortFunc(llmSpans, func(a, b tracetest.SpanStub) int {
		return int(spanAttribute(t, a, telemetry.AttrLLMSeq).AsInt64() -
			spanAttribute(t, b, telemetry.AttrLLMSeq).AsInt64())
	})
	wantRequestModels := []string{testModel, testActual}
	for index, span := range llmSpans {
		requireIntAttribute(t, span, telemetry.AttrLLMSeq, int64(index+1))
		requireStringAttribute(t, span, telemetry.AttrModel, wantRequestModels[index])
		requireStringAttribute(t, span, telemetry.AttrResponseModel, testActual)
		requireStringAttribute(t, span, telemetry.AttrPromptVersion, "v1")
		requireStringAttribute(t, span, telemetry.AttrRunID, testRunID)
		requireIntAttribute(t, span, telemetry.AttrUsageInputTokens, 10)
		requireIntAttribute(t, span, telemetry.AttrUsageOutputTokens, 5)
		requireIntAttribute(t, span, telemetry.AttrCostMicroUSD, 10000)
		requireStringAttribute(t, span, telemetry.AttrCostCurrency, "USD")
		requireHashAttribute(t, span, telemetry.AttrRequestHash)
		requireHashAttribute(t, span, telemetry.AttrResponseHash)
		requireNoAttribute(t, span, telemetry.AttrResponsePayload)
		if span.Status.Code != codes.Ok {
			t.Fatalf("llm.call status = %s, want Ok", span.Status.Code)
		}
	}

	toolSpans := spansNamed(spans, "tool.call")
	if len(toolSpans) != 3 {
		t.Fatalf("tool.call spans = %d, want 3", len(toolSpans))
	}
	slices.SortFunc(toolSpans, func(a, b tracetest.SpanStub) int {
		return int(spanAttribute(t, a, telemetry.AttrToolSeq).AsInt64() -
			spanAttribute(t, b, telemetry.AttrToolSeq).AsInt64())
	})
	wantTools := []string{"get_case", "read_thing", "propose_adjustment"}
	for index, span := range toolSpans {
		requireStringAttribute(t, span, telemetry.AttrToolName, wantTools[index])
		requireIntAttribute(t, span, telemetry.AttrToolSeq, int64(index+1))
		requireStringAttribute(t, span, telemetry.AttrRunID, testRunID)
		requireStringAttribute(t, span, telemetry.AttrCaseID, testCaseID)
		requireHashAttribute(t, span, telemetry.AttrResultHash)
		requireNoAttribute(t, span, telemetry.AttrToolErrorCode)
		requireNoAttribute(t, span, telemetry.AttrPayload)
		if span.Status.Code != codes.Ok {
			t.Fatalf("tool.call %s status = %s, want Ok", wantTools[index], span.Status.Code)
		}
	}

	// Capture is off, so no span attribute may contain prompt or memo text.
	for _, span := range spans {
		for _, attr := range span.Attributes {
			value := strings.Join(attr.Value.AsStringSlice(), ",")
			if value == "" {
				value = attr.Value.AsString()
			}
			for _, sentinel := range []string{
				"benign memo text",
				"Investigate reconciliation case " + testCaseID,
				"UNTRUSTED:field=summary",
			} {
				if strings.Contains(value, sentinel) {
					t.Fatalf("span %q attribute %q leaks raw payload %q", span.Name, attr.Key, sentinel)
				}
			}
		}
	}
}

func TestRunTracingProviderError(t *testing.T) {
	t.Parallel()

	provider, exporter := newTestTraceProvider(t)
	const secret = "super-secret-token"
	h := newHarness(t, runnerOptions{
		telemetry: provider,
		steps: []llm.ScriptedStep{{
			Err: &llm.ProviderError{
				Provider: llm.ProviderOpenCodeGo,
				Kind:     llm.KindUnavailable,
				Message:  "Authorization: Bearer " + secret,
			},
		}},
	})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v, want nil error for a persisted provider_error", err)
	}
	if result.Outcome != agent.OutcomeProviderError {
		t.Fatalf("outcome = %s, want provider_error", result.Outcome)
	}

	spans := exporter.GetSpans()
	root := singleSpan(t, spans, "case.run")
	requireStringAttribute(t, root, telemetry.AttrRunOutcome, string(agent.OutcomeProviderError))
	if root.Status.Code != codes.Unset {
		t.Fatalf("case.run status = %s, want Unset for a nil-error terminal outcome", root.Status.Code)
	}
	llmSpan := singleSpan(t, spans, "llm.call")
	if llmSpan.Status.Code != codes.Error {
		t.Fatalf("llm.call status = %s, want Error", llmSpan.Status.Code)
	}
	assertRecordedErrorRedacted(t, llmSpan, secret)
}

func TestRunTracingCassetteMiss(t *testing.T) {
	t.Parallel()

	provider, exporter := newTestTraceProvider(t)
	h := newHarness(t, runnerOptions{
		telemetry: provider,
		steps:     []llm.ScriptedStep{{Err: llm.ErrCassetteMiss}},
	})

	_, err := h.runner.Run(context.Background(), testCaseID)
	if !errors.Is(err, llm.ErrCassetteMiss) {
		t.Fatalf("Run error = %v, want ErrCassetteMiss", err)
	}

	spans := exporter.GetSpans()
	root := singleSpan(t, spans, "case.run")
	if root.Status.Code != codes.Error {
		t.Fatalf("case.run status = %s, want Error for a returned error", root.Status.Code)
	}
	requireStringAttribute(t, root, telemetry.AttrRunOutcome, string(agent.OutcomeProviderError))
	if len(root.Events) == 0 {
		t.Fatal("case.run has no recorded error event")
	}
	llmSpan := singleSpan(t, spans, "llm.call")
	if llmSpan.Status.Code != codes.Error {
		t.Fatalf("llm.call status = %s, want Error", llmSpan.Status.Code)
	}
}

func TestRunTracingToolErrorStillSpansAndFinishes(t *testing.T) {
	t.Parallel()

	provider, exporter := newTestTraceProvider(t)
	h := newHarness(t, runnerOptions{
		telemetry: provider,
		steps: []llm.ScriptedStep{
			{Response: toolResponse("call_1", "missing_tool", `{}`)},
			{Response: toolResponse("call_2", "propose_adjustment", `{}`)},
		},
	})

	result, err := h.runner.Run(context.Background(), testCaseID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != agent.OutcomeProposal {
		t.Fatalf("outcome = %s, want proposal; the unknown-tool envelope is model-visible", result.Outcome)
	}

	var failed tracetest.SpanStub
	found := false
	for _, span := range spansNamed(exporter.GetSpans(), "tool.call") {
		for _, attr := range span.Attributes {
			if string(attr.Key) == telemetry.AttrToolErrorCode && attr.Value.AsString() == "unknown_tool" {
				failed = span
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("unknown_tool tool.call span not found")
	}
	requireStringAttribute(t, failed, telemetry.AttrToolName, "unknown")
	requireStringAttribute(t, failed, telemetry.AttrToolErrorCode, "unknown_tool")
	if failed.Status.Code != codes.Error {
		t.Fatalf("unknown_tool status = %s, want Error", failed.Status.Code)
	}
	requireHashAttribute(t, failed, telemetry.AttrResultHash)
}

func TestRunTracingDisabledBehavesIdentically(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	disabled, err := telemetry.New(context.Background(), telemetry.Config{Disabled: true},
		telemetry.WithExporter(exporter), telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() {
		if err := disabled.Shutdown(context.Background()); err != nil {
			t.Fatalf("disabled provider Shutdown: %v", err)
		}
	})

	steps := func() []llm.ScriptedStep {
		return []llm.ScriptedStep{{Response: toolResponse("call_1", "escalate", `{}`)}}
	}
	untraced := newHarness(t, runnerOptions{steps: steps()})
	tracedDisabled := newHarness(t, runnerOptions{telemetry: disabled, steps: steps()})

	want, wantErr := untraced.runner.Run(context.Background(), testCaseID)
	got, gotErr := tracedDisabled.runner.Run(context.Background(), testCaseID)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("disabled-provider error = %v, want %v", gotErr, wantErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled-provider result = %+v, want %+v", got, want)
	}
	if spans := exporter.GetSpans(); len(spans) != 0 {
		t.Fatalf("disabled provider exported %d spans, want 0", len(spans))
	}
}

func assertRecordedErrorRedacted(t *testing.T, span tracetest.SpanStub, secret string) {
	t.Helper()
	if strings.Contains(span.Status.Description, secret) {
		t.Fatalf("span %q status description leaks %q: %q", span.Name, secret, span.Status.Description)
	}
	if len(span.Events) == 0 {
		t.Fatalf("span %q has no recorded error event", span.Name)
	}
	for _, event := range span.Events {
		for _, attr := range event.Attributes {
			if strings.Contains(attr.Value.AsString(), secret) {
				t.Fatalf("span %q event attribute %q leaks %q", span.Name, attr.Key, secret)
			}
		}
	}
}
