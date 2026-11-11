package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSwitchChangeForwardsActorAndTrace(t *testing.T) {
	h := newHarness(t, nil)
	h.switches.result = SwitchResult{
		Name:      SwitchAgent,
		Enabled:   false,
		Changed:   true,
		UpdatedBy: "human:dev-admin",
		UpdatedAt: time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
	}
	body := `{"name":"agent_enabled","enabled":false,"reason":"  operator pause  "}`
	response := h.request(http.MethodPost, RouteSwitches, body, "application/json", testAdminKey, func(r *http.Request) {
		r.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	})
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if h.switches.got.Name != SwitchAgent || h.switches.got.Enabled {
		t.Fatalf("request = %+v", h.switches.got)
	}
	if h.switches.got.Reason != "operator pause" {
		t.Fatalf("reason = %q, want trimmed", h.switches.got.Reason)
	}
	if h.switches.got.ActorID != "dev-admin" {
		t.Fatalf("actor = %q, want dev-admin", h.switches.got.ActorID)
	}
	if h.switches.got.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace = %q", h.switches.got.TraceID)
	}
	var result SwitchResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Changed || result.UpdatedBy != "human:dev-admin" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSwitchNoOpPassesThrough(t *testing.T) {
	h := newHarness(t, nil)
	h.switches.result = SwitchResult{
		Name:      SwitchAutoApply,
		Enabled:   true,
		Changed:   false,
		UpdatedBy: "human:dev-admin",
		UpdatedAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC),
	}
	body := `{"name":"auto_apply_enabled","enabled":true,"reason":"already on"}`
	response := h.expectStatus(http.MethodPost, RouteSwitches, body, "application/json", testAdminKey, http.StatusOK)
	defer func() { _ = response.Body.Close() }()
	var result SwitchResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Changed {
		t.Fatal("changed = true for a no-op")
	}
	if !result.UpdatedAt.Equal(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated_at = %v", result.UpdatedAt)
	}
}

func TestSwitchInvalidRequests(t *testing.T) {
	longReason := strings.Repeat("x", 501)
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"unknown name", `{"name":"other","enabled":false,"reason":"x"}`, codeInvalidRequest},
		{"missing name", `{"enabled":false,"reason":"x"}`, codeInvalidRequest},
		{"null name", `{"name":null,"enabled":false,"reason":"x"}`, codeInvalidRequest},
		{"missing enabled", `{"name":"agent_enabled","reason":"x"}`, codeInvalidRequest},
		{"null enabled", `{"name":"agent_enabled","enabled":null,"reason":"x"}`, codeInvalidRequest},
		{"string enabled", `{"name":"agent_enabled","enabled":"false","reason":"x"}`, codeInvalidRequest},
		{"missing reason", `{"name":"agent_enabled","enabled":false}`, codeInvalidRequest},
		{"empty reason", `{"name":"agent_enabled","enabled":false,"reason":"   "}`, codeInvalidRequest},
		{"oversized reason", `{"name":"agent_enabled","enabled":false,"reason":"` + longReason + `"}`, codeInvalidRequest},
		{"unknown field", `{"name":"agent_enabled","enabled":false,"reason":"x","actor":"me"}`, codeInvalidJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			response := h.expectStatus(http.MethodPost, RouteSwitches, test.body, "application/json", testAdminKey, http.StatusBadRequest)
			defer func() { _ = response.Body.Close() }()
			assertErrorCode(t, response, test.wantCode)
			if h.switches.calls != 0 {
				t.Fatal("switch service was called for an invalid request")
			}
		})
	}
}

func TestSwitchServiceFailureIsGeneric500(t *testing.T) {
	h := newHarness(t, nil)
	h.switches.err = errInternalForTest{}
	body := `{"name":"agent_enabled","enabled":false,"reason":"pause"}`
	response := h.expectStatus(http.MethodPost, RouteSwitches, body, "application/json", testAdminKey, http.StatusInternalServerError)
	bodyText := bodyOf(t, response)
	if strings.Contains(bodyText, "secret-detail") {
		t.Fatalf("service error leaked: %s", bodyText)
	}
	assertErrorCodeFromText(t, bodyText, codeInternal)
}
