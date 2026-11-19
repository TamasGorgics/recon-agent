//go:build integration

// covers: FIN-41, FIN-24, FIN-30

package lifecycle_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/bizdays"
	"github.com/TamasGorgics/recon-agent/internal/cases/lifecycle"
	"github.com/TamasGorgics/recon-agent/internal/config"
)

// slaFixture extends the shared lifecycle fixture with source scopes, breaks,
// and a lifecycle service wired to FIN-41 calendars.
type slaFixture struct {
	*fixture
	service *lifecycle.Service
	cfg     config.IngestConfig
	scopes  map[string]string
	runs    map[string]string
}

func newSLAFixture(t *testing.T, cfg config.IngestConfig, businessDays int) *slaFixture {
	t.Helper()
	f := &slaFixture{fixture: newFixture(t), cfg: cfg, scopes: map[string]string{}, runs: map[string]string{}}
	calendars, err := bizdays.NewSources(cfg)
	if err != nil {
		t.Fatalf("bizdays.NewSources: %v", err)
	}
	f.service, err = lifecycle.NewService(lifecycle.Options{
		DB:                               f.app,
		Clock:                            f.clock,
		Sources:                          calendars,
		MissingSettlementSLABusinessDays: businessDays,
	})
	if err != nil {
		t.Fatalf("lifecycle.NewService: %v", err)
	}
	sources := make([]string, 0, len(cfg.Sources))
	for source := range cfg.Sources {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		f.scopes[source], f.runs[source] = f.insertSourceScope(t, source)
	}
	return f
}

// insertSourceScope creates one account, reconciliation scope, and succeeded
// match run for a source. Scopes are unique per (source, currency).
func (f *slaFixture) insertSourceScope(t *testing.T, source string) (string, string) {
	t.Helper()
	ctx := context.Background()
	var accountID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, 'asset', 'USD', 'active') RETURNING id::text`,
		"sla_"+source+"_clearing").Scan(&accountID); err != nil {
		t.Fatalf("insert account for %s: %v", source, err)
	}
	var scopeID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, 'USD') RETURNING id::text`,
		accountID, source).Scan(&scopeID); err != nil {
		t.Fatalf("insert scope for %s: %v", source, err)
	}
	var runID string
	if err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', $3) RETURNING id::text`,
		scopeID, f.clock.Now(), "sla-"+source).Scan(&runID); err != nil {
		t.Fatalf("insert match run for %s: %v", source, err)
	}
	return scopeID, runID
}

// insertBreak attaches one break in source to caseID. Void breaks still count
// as attached evidence for the anchor and source derivation.
func (f *slaFixture) insertBreak(t *testing.T, caseID, source, side, status string,
	detectedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	scopeID, ok := f.scopes[source]
	if !ok {
		t.Fatalf("no scope configured for source %q", source)
	}
	n := fixtureSeq.Add(1)
	ledgerIDs := "ARRAY[]::uuid[]"
	refs := "[]"
	switch side {
	case "ledger_only":
		ledgerIDs = "ARRAY[gen_random_uuid()]"
	case "external_only":
		refs = fmt.Sprintf(`[{"source":%q,"source_record_id":"sla-%d"}]`, source, n)
	}
	var breakID string
	err := f.db.Pool.QueryRow(ctx,
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, `+ledgerIDs+`,
		         $5::jsonb, 0, 'USD', $6, $7)
		 RETURNING id::text`,
		fmt.Sprintf("%064x", n), scopeID, f.runs[source], side, refs, status,
		detectedAt).Scan(&breakID)
	if err != nil {
		t.Fatalf("insert break: %v", err)
	}
	if _, err := f.db.Pool.Exec(ctx,
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`, caseID, breakID, detectedAt); err != nil {
		t.Fatalf("attach break: %v", err)
	}
	return breakID
}

func (f *slaFixture) sweep(t *testing.T, now time.Time) lifecycle.SweepResult {
	t.Helper()
	result, err := f.service.EscalateMissingSettlementSLA(context.Background(), now)
	if err != nil {
		t.Fatalf("EscalateMissingSettlementSLA: %v", err)
	}
	return result
}

func auditsWithAction(rows []auditRow, action string) []auditRow {
	var out []auditRow
	for _, row := range rows {
		if row.Action == action {
			out = append(out, row)
		}
	}
	return out
}

func testCalendarConfig() config.IngestConfig {
	return config.IngestConfig{Sources: map[string]config.SourceConfig{
		"psp_a": {
			Timezone: "UTC",
			Cutoff:   "17:00",
		},
		"bank_x": {
			Timezone: "UTC",
			Cutoff:   "17:00",
		},
		"ny_holiday": {
			Timezone: "America/New_York",
			Cutoff:   "17:00",
			Holidays: []string{"2026-11-11"},
		},
		"utc_leap": {
			Timezone: "UTC",
			Cutoff:   "17:00",
		},
	}}
}

func ts(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// TestEscalateMissingSettlementSLAEscalatesCandidateStatuses proves the four
// candidate statuses escalate once, escalated/resolved stay untouched, and a
// second sweep is a no-op with no duplicate audit.
func TestEscalateMissingSettlementSLAEscalatesCandidateStatuses(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 5)
	// Monday before the cutoff; five business days later is Monday 9 Nov.
	anchor := ts(t, "2026-11-02T10:00:00Z")
	now := ts(t, "2026-11-10T12:00:00Z") // Tuesday: due date has passed

	ids := map[string]string{}
	for _, status := range []string{"open", "in_agent", "proposal_pending", "waiting_info"} {
		id := f.seedCase(t, status, "")
		f.insertBreak(t, id, "psp_a", "ledger_only", "open", anchor)
		ids[status] = id
	}
	for _, status := range []string{"escalated", "resolved"} {
		id := f.seedCase(t, status, "")
		f.insertBreak(t, id, "psp_a", "ledger_only", "open", anchor)
		ids[status] = id
	}

	result := f.sweep(t, now)
	if result.Escalated != 4 {
		t.Fatalf("first sweep = %+v, want 4 escalations", result)
	}
	for _, status := range []string{"open", "in_agent", "proposal_pending", "waiting_info"} {
		stored := f.caseRow(t, ids[status])
		if stored.Status != "escalated" || stored.Version != 1 {
			t.Fatalf("%s case = %+v, want escalated version 1", status, stored)
		}
		if stored.EscalationReason == nil || *stored.EscalationReason != "missing_settlement_sla" {
			t.Fatalf("%s escalation_reason = %v, want missing_settlement_sla", status, stored.EscalationReason)
		}
		transitions := auditsWithAction(f.audits(t, ids[status]), "case.transition")
		if len(transitions) != 1 {
			t.Fatalf("%s case.transition audits = %d, want exactly 1", status, len(transitions))
		}
		row := transitions[0]
		if row.ActorType != "system" || row.ActorID != "case-lifecycle" {
			t.Fatalf("%s audit actor = %s:%s, want system:case-lifecycle", status, row.ActorType, row.ActorID)
		}
		after := jsonObject(t, row.After)
		if after["status"] != "escalated" || after["reason"] != "missing_settlement_sla" {
			t.Fatalf("%s audit after = %v", status, after)
		}
		before := jsonObject(t, row.Before)
		if before["status"] != status {
			t.Fatalf("%s audit before = %v", status, before)
		}
	}

	for _, status := range []string{"escalated", "resolved"} {
		stored := f.caseRow(t, ids[status])
		if stored.Status != status || stored.Version != 0 {
			t.Fatalf("%s case changed: %+v", status, stored)
		}
	}

	second := f.sweep(t, now)
	if second.Escalated != 0 || second.MixedSource != 0 {
		t.Fatalf("second sweep = %+v, want no changes", second)
	}
	for _, id := range ids {
		if got := len(auditsWithAction(f.audits(t, id), "case.transition")); got > 1 {
			t.Fatalf("case %s has %d transition audits after the second sweep", id, got)
		}
	}
}

// TestEscalateMissingSettlementSLAClassifierExclusions proves non-ledger-only
// breaks exclude a case, void breaks do not, mixed sources are audited once
// and never escalated, and cases without breaks are not selected.
func TestEscalateMissingSettlementSLAClassifierExclusions(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 1)
	anchor := ts(t, "2026-11-02T10:00:00Z")
	now := ts(t, "2026-11-10T12:00:00Z")

	nonLedger := f.seedCase(t, "open", "")
	f.insertBreak(t, nonLedger, "psp_a", "ledger_only", "open", anchor)
	f.insertBreak(t, nonLedger, "psp_a", "external_only", "open", anchor)

	voidAllows := f.seedCase(t, "open", "")
	f.insertBreak(t, voidAllows, "psp_a", "ledger_only", "in_case", anchor)
	f.insertBreak(t, voidAllows, "psp_a", "external_only", "void", anchor)

	noBreaks := f.seedCase(t, "open", "")

	mixed := f.seedCase(t, "open", "")
	f.insertBreak(t, mixed, "psp_a", "ledger_only", "open", anchor)
	f.insertBreak(t, mixed, "bank_x", "ledger_only", "open", anchor)

	result := f.sweep(t, now)
	if result.Escalated != 1 {
		t.Fatalf("sweep = %+v, want exactly the void-relaxed case escalated", result)
	}
	if result.MixedSource != 1 {
		t.Fatalf("mixed-source count = %d, want 1", result.MixedSource)
	}
	if stored := f.caseRow(t, nonLedger); stored.Status != "open" || stored.Version != 0 {
		t.Fatalf("non-ledger-only case changed: %+v", stored)
	}
	if stored := f.caseRow(t, noBreaks); stored.Status != "open" || stored.Version != 0 {
		t.Fatalf("no-break case changed: %+v", stored)
	}
	if stored := f.caseRow(t, mixed); stored.Status != "open" || stored.Version != 0 {
		t.Fatalf("mixed-source case changed: %+v", stored)
	}
	if stored := f.caseRow(t, voidAllows); stored.Status != "escalated" {
		t.Fatalf("void-relaxed case status = %s, want escalated", stored.Status)
	}

	mixedAudits := auditsWithAction(f.audits(t, mixed), "case.sla_skipped_mixed_source")
	if len(mixedAudits) != 1 {
		t.Fatalf("mixed-source audits = %d, want exactly 1", len(mixedAudits))
	}
	after := jsonObject(t, mixedAudits[0].After)
	if after["skipped_reason"] != "mixed_source" {
		t.Fatalf("mixed-source audit after = %v", after)
	}
	sources, ok := after["sources"].([]any)
	if !ok || len(sources) != 2 {
		t.Fatalf("mixed-source audit sources = %v, want both sources", after["sources"])
	}

	again := f.sweep(t, now)
	if again.MixedSource != 1 {
		t.Fatalf("second sweep mixed count = %d, want 1 (deduplicated audit)", again.MixedSource)
	}
	if got := len(auditsWithAction(f.audits(t, mixed), "case.sla_skipped_mixed_source")); got != 1 {
		t.Fatalf("mixed-source audits after second sweep = %d, want 1", got)
	}
}

// TestEscalateMissingSettlementSLAUsesEarliestDetectedAt proves insertion order
// does not choose the anchor: the older break decides the due date.
func TestEscalateMissingSettlementSLAUsesEarliestDetectedAt(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 5)
	earliest := ts(t, "2026-11-02T10:00:00Z") // due Mon 9 Nov
	latest := ts(t, "2026-11-13T10:00:00Z")   // due Fri 20 Nov
	now := ts(t, "2026-11-10T12:00:00Z")      // only past the earliest due date

	id := f.seedCase(t, "open", "")
	f.insertBreak(t, id, "psp_a", "ledger_only", "open", latest)
	f.insertBreak(t, id, "psp_a", "ledger_only", "in_case", earliest)

	result := f.sweep(t, now)
	if result.Escalated != 1 {
		t.Fatalf("sweep = %+v, want the earliest-detected_at case escalated", result)
	}
	if stored := f.caseRow(t, id); stored.Status != "escalated" {
		t.Fatalf("case status = %s, want escalated", stored.Status)
	}
}

// TestEscalateMissingSettlementSLACutoffAndDueBoundary proves an anchor before
// the cutoff starts on its own date, an anchor at the cutoff starts on the next
// business day, and the due date itself never escalates.
func TestEscalateMissingSettlementSLACutoffAndDueBoundary(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 1)

	before := f.seedCase(t, "open", "")
	f.insertBreak(t, before, "psp_a", "ledger_only", "open", ts(t, "2026-11-02T16:59:00Z"))
	at := f.seedCase(t, "open", "")
	f.insertBreak(t, at, "psp_a", "ledger_only", "open", ts(t, "2026-11-02T17:00:00Z"))

	// Before-cutoff anchor: due Tuesday 3 Nov.
	result := f.sweep(t, ts(t, "2026-11-03T12:00:00Z"))
	if result.Escalated != 0 {
		t.Fatalf("sweep on the due date = %+v, want no escalation", result)
	}
	if stored := f.caseRow(t, before); stored.Version != 0 {
		t.Fatalf("case escalated on its due date: %+v", stored)
	}

	// At-cutoff anchor: start Tuesday, due Wednesday 4 Nov. A sweep on Tuesday
	// would escalate only the before-cutoff case; that proves the cutoff shift.
	result = f.sweep(t, ts(t, "2026-11-04T12:00:00Z"))
	if result.Escalated != 1 {
		t.Fatalf("sweep after the before-cutoff due = %+v, want one escalation", result)
	}
	if stored := f.caseRow(t, before); stored.Status != "escalated" {
		t.Fatalf("before-cutoff case status = %s, want escalated", stored.Status)
	}
	if stored := f.caseRow(t, at); stored.Status != "open" {
		t.Fatalf("at-cutoff case escalated on its due date: %+v", stored)
	}

	result = f.sweep(t, ts(t, "2026-11-05T12:00:00Z"))
	if result.Escalated != 1 {
		t.Fatalf("sweep after the at-cutoff due = %+v, want one escalation", result)
	}
	if stored := f.caseRow(t, at); stored.Status != "escalated" {
		t.Fatalf("at-cutoff case status = %s, want escalated", stored.Status)
	}
}

// TestEscalateMissingSettlementSLACalendarEdges proves a configured holiday
// and the DST spring-forward week drive the due date.
func TestEscalateMissingSettlementSLACalendarEdges(t *testing.T) {
	t.Run("configured holiday", func(t *testing.T) {
		f := newSLAFixture(t, testCalendarConfig(), 3)
		// Friday 6 Nov before cutoff, three business days with Wed 11 Nov a
		// holiday: Mon 9 (1), Tue 10 (2), Thu 12 (3). Due Thu 12 Nov.
		id := f.seedCase(t, "open", "")
		f.insertBreak(t, id, "ny_holiday", "ledger_only", "open", ts(t, "2026-11-06T15:00:00Z"))

		if result := f.sweep(t, ts(t, "2026-11-12T12:00:00Z")); result.Escalated != 0 {
			t.Fatalf("sweep on holiday due date = %+v, want no escalation", result)
		}
		if result := f.sweep(t, ts(t, "2026-11-13T12:00:00Z")); result.Escalated != 1 {
			t.Fatalf("sweep after holiday due = %+v, want one escalation", result)
		}
		if stored := f.caseRow(t, id); stored.Status != "escalated" {
			t.Fatalf("holiday case status = %s, want escalated", stored.Status)
		}
	})

	t.Run("DST spring-forward", func(t *testing.T) {
		f := newSLAFixture(t, testCalendarConfig(), 3)
		// Friday 6 Mar 17:00 EST exactly at cutoff: starts Mon 9 Mar (DST
		// starts Sunday), three business days later is Thu 12 Mar.
		id := f.seedCase(t, "open", "")
		f.insertBreak(t, id, "ny_holiday", "ledger_only", "open", ts(t, "2026-03-06T22:00:00Z"))

		if result := f.sweep(t, ts(t, "2026-03-12T12:00:00Z")); result.Escalated != 0 {
			t.Fatalf("sweep on DST due date = %+v, want no escalation", result)
		}
		if result := f.sweep(t, ts(t, "2026-03-13T12:00:00Z")); result.Escalated != 1 {
			t.Fatalf("sweep after DST due = %+v, want one escalation", result)
		}
		if stored := f.caseRow(t, id); stored.Status != "escalated" {
			t.Fatalf("DST case status = %s, want escalated", stored.Status)
		}
	})
}

// TestEscalateMissingSettlementSLALeapDay proves 29 Feb is a business day.
func TestEscalateMissingSettlementSLALeapDay(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 1)
	anchor := ts(t, "2024-02-28T10:00:00Z")

	id := f.seedCase(t, "open", "")
	f.insertBreak(t, id, "utc_leap", "ledger_only", "open", anchor)

	if result := f.sweep(t, ts(t, "2024-02-29T12:00:00Z")); result.Escalated != 0 {
		t.Fatalf("sweep on leap-day due date = %+v, want no escalation", result)
	}
	if result := f.sweep(t, ts(t, "2024-03-01T12:00:00Z")); result.Escalated != 1 {
		t.Fatalf("sweep after leap-day due = %+v, want one escalation", result)
	}
	if stored := f.caseRow(t, id); stored.Status != "escalated" {
		t.Fatalf("leap-day case status = %s, want escalated", stored.Status)
	}
}

// TestEscalateMissingSettlementSLAConcurrentSweepsEscalateOnce proves two
// concurrent sweeps cannot double-escalate and the second sweep is a no-op.
func TestEscalateMissingSettlementSLAConcurrentSweepsEscalateOnce(t *testing.T) {
	f := newSLAFixture(t, testCalendarConfig(), 1)
	anchor := ts(t, "2026-11-02T10:00:00Z")
	now := ts(t, "2026-11-10T12:00:00Z")

	ids := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		id := f.seedCase(t, "open", "")
		f.insertBreak(t, id, "psp_a", "ledger_only", "open", anchor)
		ids = append(ids, id)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []lifecycle.SweepResult
		errs    []error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.service.EscalateMissingSettlementSLA(context.Background(), now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, result)
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent sweeps: %v", errs)
	}
	escalated := 0
	for _, result := range results {
		escalated += result.Escalated
	}
	if escalated != len(ids) {
		t.Fatalf("concurrent escalations = %d, want %d (results %+v)", escalated, len(ids), results)
	}
	for _, id := range ids {
		stored := f.caseRow(t, id)
		if stored.Status != "escalated" || stored.Version != 1 {
			t.Fatalf("case %s = %+v, want escalated version 1", id, stored)
		}
		if got := len(auditsWithAction(f.audits(t, id), "case.transition")); got != 1 {
			t.Fatalf("case %s transition audits = %d, want 1", id, got)
		}
	}
	if result := f.sweep(t, now); result.Escalated != 0 {
		t.Fatalf("third sweep = %+v, want a no-op", result)
	}
}

// TestEscalateMissingSettlementSLABoundedBatches proves the sweep drains more
// than one default batch of 100 cases.
func TestEscalateMissingSettlementSLABoundedBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("batch test seeds more than one default batch")
	}
	f := newSLAFixture(t, testCalendarConfig(), 1)
	anchor := ts(t, "2026-11-02T10:00:00Z")
	now := ts(t, "2026-11-10T12:00:00Z")

	const cases = 105
	ids := make([]string, 0, cases)
	for i := 0; i < cases; i++ {
		id := f.seedCase(t, "open", "")
		f.insertBreak(t, id, "psp_a", "ledger_only", "open", anchor)
		ids = append(ids, id)
	}
	result := f.sweep(t, now)
	if result.Escalated != cases {
		t.Fatalf("sweep = %+v, want %d escalations across batches", result, cases)
	}
	if result.Batches < 2 {
		t.Fatalf("sweep batches = %d, want at least 2", result.Batches)
	}
	for _, id := range ids {
		if stored := f.caseRow(t, id); stored.Status != "escalated" {
			t.Fatalf("case %s = %+v, want escalated", id, stored)
		}
	}
}
