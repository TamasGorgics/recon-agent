//go:build integration

// covers: FIN-53
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// TestAPIAutoApplyResetIntegration is the FIN-53 reset evidence: the seeded
// auto_apply_enabled switch starts false, a viewer is rejected before the
// switch writer runs, and an admin reset enables auto-apply with the normal
// human switch audit. A following limits.Check then passes.
func TestAPIAutoApplyResetIntegration(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	const body = `{"name":"auto_apply_enabled","enabled":true,"reason":"operator reset"}`

	var enabled bool
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = 'auto_apply_enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("read seeded switch: %v", err)
	}
	if enabled {
		t.Fatal("seeded auto_apply_enabled is true, want false")
	}

	countAudit := func() int {
		t.Helper()
		var count int
		if err := f.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_log
			  WHERE action = 'switch.set' AND entity_type = 'system_switch'
			    AND entity_id = 'auto_apply_enabled'`).Scan(&count); err != nil {
			t.Fatalf("count switch audit: %v", err)
		}
		return count
	}

	// Viewer: 403 before body parsing/service invocation; no switch or audit
	// write.
	request, err := http.NewRequest(http.MethodPost, RouteSwitches, strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("new viewer request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testViewerKey)
	request.Header.Set("Content-Type", "application/json")
	recorder := newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", response.StatusCode)
	}
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = 'auto_apply_enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("re-read switch after viewer: %v", err)
	}
	if enabled {
		t.Fatal("viewer request changed the switch")
	}
	if got := countAudit(); got != 0 {
		t.Fatalf("viewer request wrote %d switch audits", got)
	}

	// Admin reset succeeds with the normal human actor and one audit entry.
	request, err = http.NewRequest(http.MethodPost, RouteSwitches, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new admin request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testAdminKey)
	request.Header.Set("Content-Type", "application/json")
	recorder = newTestRecorder()
	f.api.Handler().ServeHTTP(recorder, request)
	response = recorder.Result()
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", response.StatusCode)
	}
	var result SwitchResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode switch result: %v", err)
	}
	if !result.Changed || !result.Enabled || result.UpdatedBy != "human:dev-admin" {
		t.Fatalf("reset result = %+v", result)
	}

	var updatedBy string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT enabled, updated_by FROM system_switches WHERE name = 'auto_apply_enabled'`).
		Scan(&enabled, &updatedBy); err != nil {
		t.Fatalf("read reset switch: %v", err)
	}
	if !enabled || updatedBy != "human:dev-admin" {
		t.Fatalf("switch after reset = enabled:%v updated_by:%q", enabled, updatedBy)
	}
	if got := countAudit(); got != 1 {
		t.Fatalf("admin reset audits = %d, want 1", got)
	}
	var actorType, actorID string
	if err := f.db.Pool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit_log
		  WHERE action = 'switch.set' AND entity_type = 'system_switch'
		    AND entity_id = 'auto_apply_enabled'`).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("read reset audit: %v", err)
	}
	if actorType != "human" || actorID != "dev-admin" {
		t.Fatalf("reset audit actor = %s:%s, want human:dev-admin", actorType, actorID)
	}

	// A following T0 check sees the reset switch and passes with no daily
	// activity.
	svc, err := limits.NewService(limits.Options{
		Pool:   f.app.Pool,
		Clock:  clock.NewFake(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)),
		Config: config.DefaultAutoApplyLimitsConfig(),
	})
	if err != nil {
		t.Fatalf("limits.NewService: %v", err)
	}
	check, err := svc.Check(ctx, limits.Candidate{
		ProposalID: "11111111-1111-4111-8111-111111111111",
		CaseID:     "reset-evidence",
		Currency:   "USD",
		ValueMinor: 100,
		Tier:       policy.TierT0,
	})
	if err != nil {
		t.Fatalf("limits.Check after reset: %v", err)
	}
	if !check.Allowed || check.Tier != policy.TierT0 || check.Reason != limits.ReasonWithinLimits {
		t.Fatalf("Check after reset = %+v, want allowed within_limits", check)
	}
	if !check.SwitchEnabled {
		t.Fatalf("Check after reset = %+v, want switch enabled", check)
	}
}
