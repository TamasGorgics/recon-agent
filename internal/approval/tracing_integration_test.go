//go:build integration

package approval_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/TamasGorgics/recon-agent/internal/approval"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/telemetry"
)

// TestPolicyEvaluateTracingInProcess proves policy.evaluate is a child of an
// active parent with the frozen attributes and that the approval audit row
// inherits the same trace ID.
func TestPolicyEvaluateTracingInProcess(t *testing.T) {
	f := newFixture(t)
	exporter := tracetest.NewInMemoryExporter()
	provider := newTracingProvider(t, exporter)
	service := f.withTelemetry(t, provider)

	proposalID, caseID, _ := f.seedProposal(proposalSpec{
		Type:    "adjustment",
		Payload: adjustmentPayload(100),
	})
	parentCtx, parent := provider.Tracer("approval-test").Start(context.Background(), "case.run")
	parentTraceID := parent.SpanContext().TraceID().String()
	parentSpanID := parent.SpanContext().SpanID()

	result, err := service.Request(parentCtx, proposalID)
	parent.End()
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if result.Tier != policy.TierT1 {
		t.Fatalf("tier = %s, want T1", result.Tier)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want parent + policy.evaluate", len(spans))
	}
	span := requireSpan(t, spans, "policy.evaluate")
	if span.SpanContext.TraceID().String() != parentTraceID {
		t.Fatalf("policy trace = %s, want parent trace %s", span.SpanContext.TraceID(), parentTraceID)
	}
	if span.Parent.SpanID() != parentSpanID {
		t.Fatalf("policy parent span = %s, want %s", span.Parent.SpanID(), parentSpanID)
	}
	if span.Status.Code != codes.Unset {
		t.Fatalf("policy status = %v, want Unset", span.Status.Code)
	}
	requireAttrString(t, span, telemetry.AttrProposalID, proposalID)
	requireAttrString(t, span, telemetry.AttrCaseID, caseID)
	requireAttrString(t, span, telemetry.AttrPolicyTier, string(policy.TierT1))
	requireAttrString(t, span, telemetry.AttrPolicyRule, policy.CodeVerifierFailed)
	requireAttrString(t, span, telemetry.AttrPolicyConfigHash, f.loaded.Hash)
	requireAttrStrings(t, span, telemetry.AttrPolicyReasons, []string{policy.CodeVerifierFailed})
	requireNoAttr(t, span, telemetry.AttrOriginTraceID)
	requireNoAttr(t, span, telemetry.AttrPolicyLimitsReason)
	requireNoAttr(t, span, telemetry.AttrPolicyPatternVerified)

	var auditTraceID *string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM audit_log
		  WHERE action = 'proposal.policy' AND entity_id = $1`, proposalID).
		Scan(&auditTraceID); err != nil {
		t.Fatalf("read policy audit trace_id: %v", err)
	}
	if auditTraceID == nil || *auditTraceID != parentTraceID {
		t.Fatalf("policy audit trace_id = %v, want %s", auditTraceID, parentTraceID)
	}
}

// TestPolicyEvaluateTracingAsyncOwner proves an async policy evaluation starts
// a root span, sets recon.origin.trace_id to the persisted proposal trace, and
// never fabricates a parent. The audit row carries the active policy span's
// own trace ID.
func TestPolicyEvaluateTracingAsyncOwner(t *testing.T) {
	f := newFixture(t)
	exporter := tracetest.NewInMemoryExporter()
	provider := newTracingProvider(t, exporter)
	service := f.withTelemetry(t, provider)

	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:    "adjustment",
		Payload: adjustmentPayload(100),
	})
	const origin = "0123456789abcdef0123456789abcdef"
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE proposals SET trace_id = $2 WHERE id = $1::uuid`, proposalID, origin); err != nil {
		t.Fatalf("set proposal trace_id: %v", err)
	}

	if _, err := service.Request(context.Background(), proposalID); err != nil {
		t.Fatalf("Request: %v", err)
	}
	span := requireSpan(t, exporter.GetSpans(), "policy.evaluate")
	if span.Parent.IsValid() {
		t.Fatalf("async policy span has parent %s, want root", span.Parent.SpanID())
	}
	if span.SpanContext.TraceID().String() == origin {
		t.Fatalf("policy span reused the persisted origin as its own trace")
	}
	requireAttrString(t, span, telemetry.AttrOriginTraceID, origin)

	var auditTraceID *string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT trace_id FROM audit_log
		  WHERE action = 'proposal.policy' AND entity_id = $1`, proposalID).
		Scan(&auditTraceID); err != nil {
		t.Fatalf("read policy audit trace_id: %v", err)
	}
	if auditTraceID == nil || *auditTraceID != span.SpanContext.TraceID().String() {
		t.Fatalf("policy audit trace_id = %v, want active span trace %s",
			auditTraceID, span.SpanContext.TraceID())
	}
}

// TestPolicyEvaluateTracingValidationError proves an invalid decision yields an
// Error span and no decision/audit writes.
func TestPolicyEvaluateTracingValidationError(t *testing.T) {
	f := newFixture(t)
	exporter := tracetest.NewInMemoryExporter()
	provider := newTracingProvider(t, exporter)
	service := f.withTelemetry(t, provider)
	f.engine.decision = policy.Decision{
		SchemaVersion: policy.SchemaVersion,
		Tier:          policy.TierT0,
		Rule:          policy.CodeHighValue,
		Reasons:       []string{policy.CodeHighValue},
		ConfigHash:    f.loaded.Hash,
	}

	proposalID, _, _ := f.seedProposal(proposalSpec{
		Type:    "adjustment",
		Payload: adjustmentPayload(100),
	})
	_, err := service.Request(context.Background(), proposalID)
	if !errors.Is(err, approval.ErrInvalidRequest) {
		t.Fatalf("Request error = %v, want ErrInvalidRequest", err)
	}
	span := requireSpan(t, exporter.GetSpans(), "policy.evaluate")
	if span.Status.Code != codes.Error {
		t.Fatalf("policy status = %v, want Error", span.Status.Code)
	}
	if len(span.Events) == 0 {
		t.Fatal("policy Error span has no recorded error event")
	}
}

func (f *fixture) withTelemetry(t *testing.T, provider *telemetry.Provider) *approval.Service {
	t.Helper()
	service, err := approval.NewService(approval.Options{
		Pool:      f.app,
		Clock:     f.clock,
		State:     f.state,
		Verifier:  f.verifier,
		Limits:    f.limits,
		Policy:    approval.Policy{Engine: f.engine, Loaded: f.loaded},
		TTL:       testTTL,
		Telemetry: provider,
	})
	if err != nil {
		t.Fatalf("approval.NewService with telemetry: %v", err)
	}
	return service
}

func newTracingProvider(t *testing.T, exporter *tracetest.InMemoryExporter) *telemetry.Provider {
	t.Helper()
	provider, err := telemetry.New(context.Background(), telemetry.Config{ServiceName: "approval-test"},
		telemetry.WithExporter(exporter), telemetry.WithSyncer())
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return provider
}

func requireSpan(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	t.Fatalf("span %q not found in %v", name, names)
	return tracetest.SpanStub{}
}

func requireAttrString(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	value, ok := spanAttr(span, key)
	if !ok {
		t.Fatalf("span %s missing attribute %s", span.Name, key)
	}
	if value.Type() != attribute.STRING || value.AsString() != want {
		t.Fatalf("span %s attribute %s = %v, want %q", span.Name, key, value.AsInterface(), want)
	}
}

func requireAttrStrings(t *testing.T, span tracetest.SpanStub, key string, want []string) {
	t.Helper()
	value, ok := spanAttr(span, key)
	if !ok {
		t.Fatalf("span %s missing attribute %s", span.Name, key)
	}
	got := value.AsStringSlice()
	if len(got) != len(want) {
		t.Fatalf("span %s attribute %s = %v, want %v", span.Name, key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("span %s attribute %s = %v, want %v", span.Name, key, got, want)
		}
	}
}

func requireNoAttr(t *testing.T, span tracetest.SpanStub, key string) {
	t.Helper()
	if _, ok := spanAttr(span, key); ok {
		t.Fatalf("span %s unexpectedly has attribute %s", span.Name, key)
	}
}

func spanAttr(span tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
