package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
)

func TestRequestIDValidIsEchoed(t *testing.T) {
	h := newHarness(t, nil)
	response := h.request(http.MethodGet, "/healthz", "", "", "", func(r *http.Request) {
		r.Header.Set("X-Request-ID", "client-request-123")
	})
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("X-Request-ID"); got != "client-request-123" {
		t.Fatalf("X-Request-ID = %q, want the incoming valid value", got)
	}
}

func TestRequestIDGeneratedForInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{"missing", nil},
		{"empty", []string{""}},
		{"space", []string{"has space"}},
		{"control", []string{"abc\x01def"}},
		{"too long", []string{strings.Repeat("a", 129)}},
		{"multiple", []string{"first", "second"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.request(http.MethodGet, "/healthz", "", "", "", func(r *http.Request) {
				for _, value := range test.values {
					r.Header.Add("X-Request-ID", value)
				}
			})
			defer func() { _ = response.Body.Close() }()
			got := response.Header.Get("X-Request-ID")
			if len(got) != 32 || !isLowerHex(got) {
				t.Fatalf("X-Request-ID = %q, want 32 lowercase hex characters", got)
			}
			for _, invalid := range test.values {
				if invalid != "" && got == invalid {
					t.Fatalf("echoed invalid request ID %q", invalid)
				}
			}
		})
	}
}

func TestTraceparentValidUsesTraceID(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	h := newHarness(t, nil)
	response := h.request(http.MethodGet, "/healthz", "", "", "", func(r *http.Request) {
		r.Header.Set("Traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	})
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("X-Trace-ID"); got != traceID {
		t.Fatalf("X-Trace-ID = %q, want %q", got, traceID)
	}
}

func TestTraceparentInvalidGeneratesTraceID(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	tests := []struct {
		name   string
		values []string
	}{
		{"version 01", []string{"01-" + traceID + "-00f067aa0ba902b7-01"}},
		{"zero trace", []string{"00-00000000000000000000000000000000-00f067aa0ba902b7-01"}},
		{"zero parent", []string{"00-" + traceID + "-0000000000000000-01"}},
		{"multiple", []string{"00-" + traceID + "-00f067aa0ba902b7-01", "00-" + traceID + "-00f067aa0ba902b7-01"}},
		{"garbage", []string{"not-a-traceparent"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.request(http.MethodGet, "/healthz", "", "", "", func(r *http.Request) {
				for _, value := range test.values {
					r.Header.Add("Traceparent", value)
				}
			})
			defer func() { _ = response.Body.Close() }()
			got := response.Header.Get("X-Trace-ID")
			if len(got) != 32 || !isLowerHex(got) {
				t.Fatalf("X-Trace-ID = %q, want generated 32 lowercase hex", got)
			}
			if got == traceID {
				t.Fatal("used the invalid traceparent trace ID")
			}
		})
	}
}

func TestRecoveryReturns500Envelope(t *testing.T) {
	h := newHarness(t, nil)
	a := h.api
	handler := a.withRequestContext(a.recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret panic detail")
	})))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/cases", nil)
	request.Header.Set("X-Request-ID", "panic-request-id")
	handler.ServeHTTP(recorder, request)

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	var envelope errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error.Code != codeInternal {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, codeInternal)
	}
	if envelope.RequestID != "panic-request-id" {
		t.Fatalf("request_id = %q, want panic-request-id", envelope.RequestID)
	}
	if strings.Contains(bodyString(recorder), "secret panic detail") {
		t.Fatal("panic detail leaked into the response")
	}
	if strings.Contains(h.logs.String(), "secret panic detail") {
		t.Fatal("panic detail leaked into the logs")
	}
}

func TestJSONBodyCapReturns413(t *testing.T) {
	h := newHarness(t, func(opts *Options) {
		opts.Config.MaxJSONBytes = 64
	})
	body := `{"scope_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","from":"2026-11-01","to":"2026-11-07","padding":"` +
		strings.Repeat("x", 128) + `"}`
	response := h.expectStatus(http.MethodPost, "/v1/reconcile/run", body, "application/json", testAdminKey, http.StatusRequestEntityTooLarge)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codePayloadTooLarge)
}

func TestAccessLogFieldsAndNoSecret(t *testing.T) {
	h := newHarness(t, nil)
	response := h.request(http.MethodGet, "/v1/cases", "", "", testViewerKey, func(r *http.Request) {
		r.Header.Set("X-Request-ID", "log-request-id")
		r.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	})
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	logs := h.logs.String()
	for _, fragment := range []string{
		`"method":"GET"`,
		`"route":"/v1/cases"`,
		`"status":200`,
		`"request_id":"log-request-id"`,
		`"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`,
		`"key_id":"dev-viewer"`,
		`"role":"viewer"`,
	} {
		if !strings.Contains(logs, fragment) {
			t.Fatalf("access log missing %s: %s", fragment, logs)
		}
	}
	if strings.Contains(logs, testViewerKey) || strings.Contains(logs, "Authorization") {
		t.Fatalf("access log leaked an authorization value: %s", logs)
	}
}

type panicOnListCaseReader struct{}

func (panicOnListCaseReader) ListCases(_ context.Context, _ CaseListQuery) (CaseListPage, error) {
	panic("access-log panic probe")
}

// TestAccessLogAfterRecoveredPanicIs500 pins the middleware order: recovery
// must commit the 500 response before the access log observes the status.
func TestAccessLogAfterRecoveredPanicIs500(t *testing.T) {
	h := newHarness(t, func(opts *Options) {
		opts.CaseReader = panicOnListCaseReader{}
	})
	response := h.expectStatus(http.MethodGet, RouteCases, "", "", testViewerKey, http.StatusInternalServerError)
	defer func() { _ = response.Body.Close() }()
	assertErrorCode(t, response, codeInternal)
	logs := h.logs.String()
	if !strings.Contains(logs, `"status":500`) {
		t.Fatalf("access log after a recovered panic does not record status 500: %s", logs)
	}
	if strings.Contains(logs, `"status":200`) {
		t.Fatalf("access log recorded a phantom 200 after a recovered panic: %s", logs)
	}
	if strings.Contains(logs, "access-log panic probe") {
		t.Fatalf("access log leaked the panic detail: %s", logs)
	}
}

func TestAccessLogUnauthenticatedHasEmptyPrincipal(t *testing.T) {
	h := newHarness(t, nil)
	response := h.expectStatus(http.MethodGet, "/v1/cases", "", "", "", http.StatusUnauthorized)
	defer func() { _ = response.Body.Close() }()
	logs := h.logs.String()
	if !strings.Contains(logs, `"key_id":""`) || !strings.Contains(logs, `"role":""`) {
		t.Fatalf("unauthenticated access log should have empty principal: %s", logs)
	}
}

func TestClockIsInjectedInFallbackID(t *testing.T) {
	h := newHarness(t, func(opts *Options) {
		opts.Clock = clock.NewFake(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC))
	})
	if h.api.clock.Now().Year() != 2020 {
		t.Fatal("API did not retain the injected clock")
	}
}

func assertErrorCode(t *testing.T, response *http.Response, want string) {
	t.Helper()
	var envelope errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error.Code != want {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, want)
	}
	if envelope.RequestID == "" {
		t.Fatal("error envelope is missing request_id")
	}
}

func bodyString(recorder *httptest.ResponseRecorder) string {
	return recorder.Body.String()
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || (value[i] > '9' && value[i] < 'a') || value[i] > 'f' {
			return false
		}
	}
	return true
}
