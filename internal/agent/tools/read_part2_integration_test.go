//go:build integration

package tools_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
)

// part2Seq keeps fixture identifiers unique inside one test database clone.
var part2Seq atomic.Int64

func part2Time(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UTC()
}

func part2Signature(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func part2InsertAccount(t *testing.T, world *integrationWorld, currency string) string {
	t.Helper()
	code := fmt.Sprintf("part2_account_%d", part2Seq.Add(1))
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO accounts (code, name, type, currency, status)
		 VALUES ($1, $1, 'asset', $2, 'active')
		 RETURNING id::text`, code, currency).Scan(&id); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func part2InsertScope(t *testing.T, world *integrationWorld, accountID, source, currency string) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO reconciliation_scopes (ledger_account_id, source, currency)
		 VALUES ($1::uuid, $2, $3)
		 RETURNING id::text`, accountID, source, currency).Scan(&id); err != nil {
		t.Fatalf("insert scope: %v", err)
	}
	return id
}

func part2InsertMatchRun(t *testing.T, world *integrationWorld, scopeID string) string {
	t.Helper()
	now := part2Time(t, "2026-01-01T00:00:00Z")
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO match_runs (scope_id, started_at, finished_at, status, stats, config_hash)
		 VALUES ($1::uuid, $2, $2, 'succeeded', '{}'::jsonb, 'part2-fixture')
		 RETURNING id::text`, scopeID, now).Scan(&id); err != nil {
		t.Fatalf("insert match run: %v", err)
	}
	return id
}

func part2InsertBatch(
	t *testing.T,
	world *integrationWorld,
	source, currency, status string,
	declaredTotal int64,
	rowCount, declaredRowCount int,
	periodStart, periodEnd *time.Time,
	quarantineReason *string,
) string {
	t.Helper()
	fileHash := part2Signature(fmt.Sprintf("part2-batch-%d", part2Seq.Add(1)))
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO settlement_batches
		     (source, period_start, period_end, declared_total_minor, currency,
		      file_hash, status, quarantine_reason, row_count, declared_row_count)
		 VALUES ($1, $2::date, $3::date, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id::text`,
		source, periodStart, periodEnd, declaredTotal, currency, fileHash,
		status, quarantineReason, rowCount, declaredRowCount).Scan(&id); err != nil {
		t.Fatalf("insert settlement batch: %v", err)
	}
	return id
}

func part2InsertExternal(
	t *testing.T,
	world *integrationWorld,
	batchID, source, recordID string,
	amountMinor int64,
	currency, raw string,
	occurredAt time.Time,
) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO external_records
		     (source, source_record_id, kind, amount_minor, currency, fee_minor,
		      occurred_at, settled_at, value_date, batch_id, raw, raw_hash)
		 VALUES ($1, $2, 'charge', $3, $4, 0, $5, $5, $6::date, $7::uuid,
		         $8::jsonb, $9)
		 RETURNING id::text`,
		source, recordID, amountMinor, currency, occurredAt,
		occurredAt.UTC().Format("2006-01-02"), batchID, raw,
		part2Signature(recordID)).Scan(&id); err != nil {
		t.Fatalf("insert external record %q: %v", recordID, err)
	}
	return id
}

func part2InsertStatementBalance(
	t *testing.T,
	world *integrationWorld,
	scopeID, batchID, currency, asOf string,
	openingMinor, closingMinor int64,
) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO statement_balances
		     (scope_id, as_of_date, opening_minor, closing_minor, currency, batch_id)
		 VALUES ($1::uuid, $2::date, $3, $4, $5, $6::uuid)
		 RETURNING id::text`,
		scopeID, asOf, openingMinor, closingMinor, currency, batchID).Scan(&id); err != nil {
		t.Fatalf("insert statement balance: %v", err)
	}
	return id
}

func part2InsertFXRate(
	t *testing.T,
	world *integrationWorld,
	base, quote, rate string,
	asOf time.Time,
	source string,
) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO fx_rates (base, quote, rate, as_of, source)
		 VALUES ($1, $2, $3::numeric, $4, $5)
		 RETURNING id::text`, base, quote, rate, asOf, source).Scan(&id); err != nil {
		t.Fatalf("insert fx rate: %v", err)
	}
	return id
}

func part2InsertFeeSchedule(
	t *testing.T,
	world *integrationWorld,
	source, kind, currency string,
	pctBps int,
	fixedMinor int64,
	validFrom string,
	validTo *string,
) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO fee_schedules
		     (source, kind, currency, pct_bps, fixed_minor, valid_from, valid_to)
		 VALUES ($1, $2, $3, $4, $5, $6::date, $7::date)
		 RETURNING id::text`,
		source, kind, currency, pctBps, fixedMinor, validFrom, validTo).Scan(&id); err != nil {
		t.Fatalf("insert fee schedule: %v", err)
	}
	return id
}

func part2InsertBreak(
	t *testing.T,
	world *integrationWorld,
	scopeID, matchRunID string,
	side, currency string,
	deltaMinor int64,
	signature string,
	detectedAt time.Time,
) string {
	t.Helper()
	refs := fmt.Sprintf(`[{"source":"psp_a","source_record_id":"part2-%s"}]`, signature[:16])
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO breaks
		     (signature, scope_id, match_run_id, side, ledger_txn_ids,
		      external_refs, delta_minor, currency, status, detected_at)
		 VALUES ($1, $2::uuid, $3::uuid, $4, '{}'::uuid[],
		         $5::jsonb, $6, $7, 'open', $8)
		 RETURNING id::text`,
		signature, scopeID, matchRunID, side, refs, deltaMinor, currency,
		detectedAt).Scan(&id); err != nil {
		t.Fatalf("insert break: %v", err)
	}
	return id
}

func part2InsertCase(t *testing.T, world *integrationWorld, valueMinor int64, currency string) string {
	t.Helper()
	var id string
	if err := world.DB.Pool.QueryRow(context.Background(),
		`INSERT INTO cases (priority, value_minor, currency, status, created_at)
		 VALUES (0, $1, $2, 'open', $3)
		 RETURNING id::text`, valueMinor, currency, part2Time(t, "2026-01-01T00:00:00Z")).Scan(&id); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	return id
}

func part2AddCaseBreak(t *testing.T, world *integrationWorld, caseID, breakID string) {
	t.Helper()
	if _, err := world.DB.Pool.Exec(context.Background(),
		`INSERT INTO case_breaks (case_id, break_id, created_at)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		caseID, breakID, part2Time(t, "2026-01-01T00:00:00Z")); err != nil {
		t.Fatalf("insert case break: %v", err)
	}
}

func part2CaseService(t *testing.T, world *integrationWorld, fake *clock.Fake) *cases.Service {
	t.Helper()
	actor, err := audit.SystemActor("tools-test")
	if err != nil {
		t.Fatalf("system actor: %v", err)
	}
	service, err := cases.NewService(world.DB.Pool, fake, actor)
	if err != nil {
		t.Fatalf("new case service: %v", err)
	}
	return service
}

func part2ResolveCase(t *testing.T, service *cases.Service, caseID, resolutionType string) {
	t.Helper()
	if _, err := service.Transition(context.Background(), cases.TransitionRequest{
		CaseID:          caseID,
		ExpectedVersion: 0,
		Status:          cases.StatusResolved,
		ResolutionType:  resolutionType,
	}); err != nil {
		t.Fatalf("resolve case %s: %v", caseID, err)
	}
}

// part2Harness registers the part-2 tools on the real agent-pool queryer and
// the app-pool recorder.
type part2Harness struct {
	runner *tools.Runner
	run    *tools.RunContext
	seq    int
}

func newPart2Harness(t *testing.T, world *integrationWorld, fake *clock.Fake) *part2Harness {
	t.Helper()
	registry := tools.NewRegistry()
	deps := tools.Deps{Agent: world.Agent, Clock: fake, UntrustedFields: nil}
	if err := tools.RegisterPart2(registry, deps); err != nil {
		t.Fatalf("RegisterPart2: %v", err)
	}
	return &part2Harness{
		runner: tools.NewRunner(registry, tools.NewDBRecorder(world.App), fake),
		run:    tools.NewRunContext(world.RunID, world.CaseID, world.Agent, fake),
	}
}

func (h *part2Harness) call(t *testing.T, name, args string) json.RawMessage {
	t.Helper()
	h.seq++
	raw, err := h.runner.Call(context.Background(), h.run, h.seq, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return raw
}

type part2Envelope[T any] struct {
	Data            T        `json:"data"`
	Refs            []string `json:"refs"`
	UntrustedFields []string `json:"untrusted_fields"`
	Truncated       bool     `json:"truncated"`
}

func part2DecodeSuccess[T any](t *testing.T, raw json.RawMessage) part2Envelope[T] {
	t.Helper()
	base := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, base); code != "" {
		t.Fatalf("unexpected %s error: %s", code, envelopeErrorMessage(t, base))
	}
	var envelope part2Envelope[T]
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode data: %v\n%s", err, raw)
	}
	return envelope
}

func part2RequireErrorCode(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	envelope := decodeEnvelope(t, raw)
	if code := envelopeErrorCode(t, envelope); code != want {
		t.Fatalf("error code = %q, want %q: %s", code, want, raw)
	}
}

func part2RefSet(refs []string) map[string]struct{} {
	set := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		set[ref] = struct{}{}
	}
	return set
}

type part2BatchRecord struct {
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	SourceRecordID  string         `json:"source_record_id"`
	Kind            string         `json:"kind"`
	AmountMinor     int64          `json:"amount_minor"`
	Currency        string         `json:"currency"`
	FeeMinor        int64          `json:"fee_minor"`
	OccurredAt      string         `json:"occurred_at"`
	SettledAt       *string        `json:"settled_at"`
	ValueDate       string         `json:"value_date"`
	Raw             map[string]any `json:"raw"`
	UntrustedFields []string       `json:"untrusted_fields"`
}

type part2StatementBalance struct {
	ID           string `json:"id"`
	ScopeID      string `json:"scope_id"`
	AsOfDate     string `json:"as_of_date"`
	OpeningMinor int64  `json:"opening_minor"`
	ClosingMinor int64  `json:"closing_minor"`
	Currency     string `json:"currency"`
	BatchID      string `json:"batch_id"`
}

type part2BatchData struct {
	ID                 string                  `json:"id"`
	Source             string                  `json:"source"`
	PeriodStart        *string                 `json:"period_start"`
	PeriodEnd          *string                 `json:"period_end"`
	DeclaredTotalMinor int64                   `json:"declared_total_minor"`
	Currency           string                  `json:"currency"`
	Status             string                  `json:"status"`
	QuarantineReason   *string                 `json:"quarantine_reason"`
	RowCount           int                     `json:"row_count"`
	DeclaredRowCount   int                     `json:"declared_row_count"`
	Records            []part2BatchRecord      `json:"records"`
	StatementBalances  []part2StatementBalance `json:"statement_balances"`
}

func TestIntegrationGetSettlementBatch(t *testing.T) {
	world := newIntegrationWorld(t)
	accountID := part2InsertAccount(t, world, "USD")
	scopeID := part2InsertScope(t, world, accountID, "psp_a", "USD")
	periodStart := part2Time(t, "2026-01-01T00:00:00Z")
	periodEnd := part2Time(t, "2026-01-05T00:00:00Z")
	batchID := part2InsertBatch(t, world, "psp_a", "USD", "ingested", 150000, 2, 2,
		&periodStart, &periodEnd, nil)
	part2InsertExternal(t, world, batchID, "psp_a", "r-001", 10000, "USD",
		`{"memo":"hello","reference":"ref-1"}`, part2Time(t, "2026-01-02T12:00:00Z"))
	part2InsertExternal(t, world, batchID, "psp_a", "r-002", 20000, "USD",
		`{"memo":"world"}`, part2Time(t, "2026-01-03T12:00:00Z"))
	part2InsertStatementBalance(t, world, scopeID, batchID, "USD", "2026-01-02", 1000, 2000)
	part2InsertStatementBalance(t, world, scopeID, batchID, "USD", "2026-01-03", 2000, 3000)

	fake := clock.NewFake(part2Time(t, "2026-01-10T00:00:00Z"))
	harness := newPart2Harness(t, world, fake)

	raw := harness.call(t, "get_settlement_batch", fmt.Sprintf(`{"batch_id":%q}`, batchID))
	envelope := part2DecodeSuccess[part2BatchData](t, raw)
	data := envelope.Data
	if data.ID != batchID || data.Source != "psp_a" || data.Currency != "USD" {
		t.Fatalf("batch header = %+v", data)
	}
	if data.PeriodStart == nil || *data.PeriodStart != "2026-01-01" {
		t.Fatalf("period_start = %v, want 2026-01-01", data.PeriodStart)
	}
	if data.PeriodEnd == nil || *data.PeriodEnd != "2026-01-05" {
		t.Fatalf("period_end = %v, want 2026-01-05", data.PeriodEnd)
	}
	if data.DeclaredTotalMinor != 150000 || data.Status != "ingested" ||
		data.QuarantineReason != nil || data.RowCount != 2 || data.DeclaredRowCount != 2 {
		t.Fatalf("batch header = %+v", data)
	}
	if envelope.Truncated {
		t.Fatal("small batch was truncated")
	}
	if len(data.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(data.Records))
	}
	first := data.Records[0]
	if first.SourceRecordID != "r-001" || first.AmountMinor != 10000 ||
		first.Currency != "USD" || first.ValueDate != "2026-01-02" {
		t.Fatalf("first record = %+v", first)
	}
	if first.Raw["memo"] != "hello" || first.Raw["reference"] != "ref-1" {
		t.Fatalf("first record raw = %v", first.Raw)
	}
	if strings.Join(first.UntrustedFields, ",") != "memo,reference" {
		t.Fatalf("first record untrusted_fields = %v", first.UntrustedFields)
	}
	if strings.Join(data.Records[1].UntrustedFields, ",") != "memo" {
		t.Fatalf("second record untrusted_fields = %v", data.Records[1].UntrustedFields)
	}
	if strings.Join(envelope.UntrustedFields, ",") != "memo,reference" {
		t.Fatalf("envelope untrusted_fields = %v", envelope.UntrustedFields)
	}
	if len(data.StatementBalances) != 2 {
		t.Fatalf("statement_balances = %d, want 2", len(data.StatementBalances))
	}
	if data.StatementBalances[0].AsOfDate != "2026-01-02" ||
		data.StatementBalances[0].OpeningMinor != 1000 ||
		data.StatementBalances[0].ClosingMinor != 2000 ||
		data.StatementBalances[0].BatchID != batchID {
		t.Fatalf("first statement balance = %+v", data.StatementBalances[0])
	}

	refs := part2RefSet(envelope.Refs)
	if len(envelope.Refs) != 3 {
		t.Fatalf("refs = %v, want 3", envelope.Refs)
	}
	for _, want := range []string{
		"batch:" + batchID,
		"ext:psp_a:r-001",
		"ext:psp_a:r-002",
	} {
		if _, ok := refs[want]; !ok {
			t.Fatalf("refs %v missing %q", envelope.Refs, want)
		}
	}

	// A canonical upper-case UUID still resolves against the text view.
	raw = harness.call(t, "get_settlement_batch",
		fmt.Sprintf(`{"batch_id":%q}`, strings.ToUpper(batchID)))
	envelope = part2DecodeSuccess[part2BatchData](t, raw)
	if envelope.Data.ID != batchID {
		t.Fatalf("upper-case id returned %q, want %q", envelope.Data.ID, batchID)
	}

	// A quarantined batch returns its header and empty member lists, never an
	// error and never member evidence.
	reason := "declared total mismatch"
	quarantinedID := part2InsertBatch(t, world, "psp_a", "USD", "quarantined", 0, 0, 3,
		&periodStart, &periodEnd, &reason)
	raw = harness.call(t, "get_settlement_batch", fmt.Sprintf(`{"batch_id":%q}`, quarantinedID))
	envelope = part2DecodeSuccess[part2BatchData](t, raw)
	if envelope.Data.Status != "quarantined" || envelope.Data.QuarantineReason == nil ||
		*envelope.Data.QuarantineReason != reason {
		t.Fatalf("quarantined header = %+v", envelope.Data)
	}
	if len(envelope.Data.Records) != 0 || len(envelope.Data.StatementBalances) != 0 {
		t.Fatalf("quarantined batch members = %+v", envelope.Data)
	}
	if envelope.Truncated || len(envelope.Refs) != 1 ||
		envelope.Refs[0] != "batch:"+quarantinedID {
		t.Fatalf("quarantined envelope = %+v", envelope)
	}

	// An ingested batch with no members is also a successful header-only
	// result, not an error.
	emptyID := part2InsertBatch(t, world, "psp_a", "USD", "ingested", 0, 0, 0,
		nil, nil, nil)
	raw = harness.call(t, "get_settlement_batch", fmt.Sprintf(`{"batch_id":%q}`, emptyID))
	envelope = part2DecodeSuccess[part2BatchData](t, raw)
	if len(envelope.Data.Records) != 0 || len(envelope.Data.StatementBalances) != 0 {
		t.Fatalf("empty batch members = %+v", envelope.Data)
	}
	if envelope.Truncated || len(envelope.Refs) != 1 ||
		envelope.Refs[0] != "batch:"+emptyID {
		t.Fatalf("empty batch envelope = %+v", envelope)
	}

	raw = harness.call(t, "get_settlement_batch",
		`{"batch_id":"99999999-9999-4999-8999-999999999999"}`)
	part2RequireErrorCode(t, raw, tools.CodeNotFound)

	// 51 members must yield the first 50 deterministically and truncated=true.
	capBatchID := part2InsertBatch(t, world, "psp_a", "USD", "ingested", 51000, 51, 51,
		&periodStart, &periodEnd, nil)
	for index := 0; index < 51; index++ {
		part2InsertExternal(t, world, capBatchID, "psp_a", fmt.Sprintf("cap-%02d", index),
			1000, "USD", `{}`, part2Time(t, "2026-01-02T12:00:00Z"))
	}
	raw = harness.call(t, "get_settlement_batch", fmt.Sprintf(`{"batch_id":%q}`, capBatchID))
	envelope = part2DecodeSuccess[part2BatchData](t, raw)
	if !envelope.Truncated {
		t.Fatal("51-member batch was not marked truncated")
	}
	if len(envelope.Data.Records) != 50 {
		t.Fatalf("records = %d, want the 50-row cap", len(envelope.Data.Records))
	}
	if envelope.Data.Records[0].SourceRecordID != "cap-00" ||
		envelope.Data.Records[49].SourceRecordID != "cap-49" {
		t.Fatalf("records are not deterministic: first=%q last=%q",
			envelope.Data.Records[0].SourceRecordID, envelope.Data.Records[49].SourceRecordID)
	}
	refs = part2RefSet(envelope.Refs)
	if len(envelope.Refs) != 51 {
		t.Fatalf("refs = %d, want batch plus 50 members", len(envelope.Refs))
	}
	if _, ok := refs["ext:psp_a:cap-50"]; ok {
		t.Fatal("dropped member contributed an evidence ref")
	}
	if _, ok := refs["ext:psp_a:cap-49"]; !ok {
		t.Fatalf("last included member has no evidence ref: %v", envelope.Refs)
	}
}

type part2FXData struct {
	Base   string `json:"base"`
	Quote  string `json:"quote"`
	Rate   string `json:"rate"`
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
}

// covers: FIN-01
func TestIntegrationGetFXRate(t *testing.T) {
	world := newIntegrationWorld(t)
	part2InsertFXRate(t, world, "USD", "EUR", "1.100000000000",
		part2Time(t, "2026-01-01T12:00:00Z"), "ecb")
	part2InsertFXRate(t, world, "USD", "EUR", "1.250000000000",
		part2Time(t, "2026-01-02T23:59:59Z"), "ecb")
	part2InsertFXRate(t, world, "USD", "EUR", "9.990000000000",
		part2Time(t, "2026-01-03T00:00:00Z"), "ecb")

	fake := clock.NewFake(part2Time(t, "2026-01-10T00:00:00Z"))
	harness := newPart2Harness(t, world, fake)

	raw := harness.call(t, "get_fx_rate",
		`{"base":"USD","quote":"EUR","date":"2026-01-02"}`)
	envelope := part2DecodeSuccess[part2FXData](t, raw)
	if envelope.Data.Rate != "1.250000000000" {
		t.Fatalf("rate = %q (%T), want the decimal string 1.250000000000",
			envelope.Data.Rate, envelope.Data.Rate)
	}
	if !strings.Contains(string(raw), `"rate":"1.250000000000"`) {
		t.Fatalf("rate is not encoded as a JSON string: %s", raw)
	}
	if envelope.Data.AsOf != "2026-01-02T23:59:59Z" {
		t.Fatalf("as_of = %q, want 2026-01-02T23:59:59Z", envelope.Data.AsOf)
	}
	if envelope.Data.Base != "USD" || envelope.Data.Quote != "EUR" ||
		envelope.Data.Source != "ecb" {
		t.Fatalf("fx data = %+v", envelope.Data)
	}
	if len(envelope.Refs) != 1 || envelope.Refs[0] != "fx:USD/EUR@2026-01-02" {
		t.Fatalf("refs = %v, want the requested-date fx ref", envelope.Refs)
	}

	raw = harness.call(t, "get_fx_rate",
		`{"base":"USD","quote":"EUR","date":"2026-01-01"}`)
	envelope = part2DecodeSuccess[part2FXData](t, raw)
	if envelope.Data.Rate != "1.100000000000" {
		t.Fatalf("rate = %q, want 1.100000000000", envelope.Data.Rate)
	}

	raw = harness.call(t, "get_fx_rate",
		`{"base":"USD","quote":"EUR","date":"2025-12-31"}`)
	part2RequireErrorCode(t, raw, tools.CodeNotFound)

	raw = harness.call(t, "get_fx_rate",
		`{"base":"USD","quote":"USD","date":"2026-01-02"}`)
	part2RequireErrorCode(t, raw, tools.CodeInvalidArguments)
}

type part2FeeData struct {
	ID         string  `json:"id"`
	Source     string  `json:"source"`
	Kind       string  `json:"kind"`
	Currency   string  `json:"currency"`
	PctBPS     int     `json:"pct_bps"`
	FixedMinor int64   `json:"fixed_minor"`
	ValidFrom  string  `json:"valid_from"`
	ValidTo    *string `json:"valid_to"`
}

func TestIntegrationGetFeeSchedule(t *testing.T) {
	world := newIntegrationWorld(t)
	boundaryID := part2InsertFeeSchedule(t, world, "psp_a", "boundary", "USD",
		100, 25, "2026-01-01", stringPtr("2026-01-31"))
	fake := clock.NewFake(part2Time(t, "2026-01-10T00:00:00Z"))
	harness := newPart2Harness(t, world, fake)

	raw := harness.call(t, "get_fee_schedule",
		`{"source":"psp_a","kind":"boundary","currency":"USD","date":"2026-01-01"}`)
	envelope := part2DecodeSuccess[part2FeeData](t, raw)
	if envelope.Data.ID != boundaryID || envelope.Data.PctBPS != 100 ||
		envelope.Data.FixedMinor != 25 {
		t.Fatalf("fee schedule = %+v, want integer pct_bps/fixed_minor", envelope.Data)
	}
	if envelope.Data.ValidFrom != "2026-01-01" || envelope.Data.ValidTo == nil ||
		*envelope.Data.ValidTo != "2026-01-31" {
		t.Fatalf("fee window = %+v", envelope.Data)
	}
	if len(envelope.Refs) != 1 || envelope.Refs[0] != "fee_schedule:"+boundaryID {
		t.Fatalf("refs = %v, want the fee schedule ref", envelope.Refs)
	}

	raw = harness.call(t, "get_fee_schedule",
		`{"source":"psp_a","kind":"boundary","currency":"USD","date":"2026-01-31"}`)
	envelope = part2DecodeSuccess[part2FeeData](t, raw)
	if envelope.Data.ID != boundaryID {
		t.Fatalf("valid_to boundary returned %q, want %q", envelope.Data.ID, boundaryID)
	}

	for _, date := range []string{"2025-12-31", "2026-02-01"} {
		raw = harness.call(t, "get_fee_schedule",
			fmt.Sprintf(`{"source":"psp_a","kind":"boundary","currency":"USD","date":%q}`, date))
		part2RequireErrorCode(t, raw, tools.CodeNotFound)
	}

	// Overlapping schedules: latest valid_from wins; an open-ended schedule
	// stays in force with no valid_to.
	olderID := part2InsertFeeSchedule(t, world, "psp_a", "overlap", "USD",
		100, 5, "2026-03-01", stringPtr("2026-04-30"))
	newerID := part2InsertFeeSchedule(t, world, "psp_a", "overlap", "USD",
		200, 7, "2026-03-15", nil)

	raw = harness.call(t, "get_fee_schedule",
		`{"source":"psp_a","kind":"overlap","currency":"USD","date":"2026-03-10"}`)
	envelope = part2DecodeSuccess[part2FeeData](t, raw)
	if envelope.Data.ID != olderID {
		t.Fatalf("2026-03-10 returned %q, want %q", envelope.Data.ID, olderID)
	}

	raw = harness.call(t, "get_fee_schedule",
		`{"source":"psp_a","kind":"overlap","currency":"USD","date":"2026-03-20"}`)
	envelope = part2DecodeSuccess[part2FeeData](t, raw)
	if envelope.Data.ID != newerID || envelope.Data.ValidTo != nil {
		t.Fatalf("overlap picked %+v, want the latest valid_from and null valid_to", envelope.Data)
	}

	raw = harness.call(t, "get_fee_schedule",
		`{"source":"psp_a","kind":"overlap","currency":"USD","date":"2026-05-01"}`)
	envelope = part2DecodeSuccess[part2FeeData](t, raw)
	if envelope.Data.ID != newerID {
		t.Fatalf("open-ended schedule returned %q, want %q", envelope.Data.ID, newerID)
	}
}

type part2HistoryMatch struct {
	ScopeID    string `json:"scope_id"`
	Side       string `json:"side"`
	Currency   string `json:"currency"`
	DeltaMinor int64  `json:"delta_minor"`
}

type part2HistoryCase struct {
	CaseID         string `json:"case_id"`
	ValueMinor     int64  `json:"value_minor"`
	Currency       string `json:"currency"`
	ResolutionType string `json:"resolution_type"`
	ResolvedAt     string `json:"resolved_at"`
	MatchingBreaks int    `json:"matching_breaks"`
}

type part2HistoryData struct {
	Signature string             `json:"signature"`
	Match     part2HistoryMatch  `json:"match"`
	Cases     []part2HistoryCase `json:"cases"`
}

func stringPtr(value string) *string { return &value }

// covers: FIN-42
func TestIntegrationGetHistoryStructuralSimilarity(t *testing.T) {
	fake := clock.NewFake(part2Time(t, "2026-01-10T00:00:00Z"))
	world := newIntegrationWorld(t)
	accountID := part2InsertAccount(t, world, "USD")
	scopeA := part2InsertScope(t, world, accountID, "psp_a", "USD")
	runA := part2InsertMatchRun(t, world, scopeA)
	accountB := part2InsertAccount(t, world, "USD")
	scopeB := part2InsertScope(t, world, accountB, "bank_x", "USD")
	runB := part2InsertMatchRun(t, world, scopeB)
	service := part2CaseService(t, world, fake)

	currentSignature := part2Signature("history-current")
	currentBreak := part2InsertBreak(t, world, scopeA, runA, "external_only", "USD",
		-1234, currentSignature, fake.Now())
	part2AddCaseBreak(t, world, world.CaseID, currentBreak)

	caseOne := part2InsertCase(t, world, 1234, "USD")
	for index := 0; index < 2; index++ {
		breakID := part2InsertBreak(t, world, scopeA, runA, "external_only", "USD",
			-1234, part2Signature(fmt.Sprintf("history-case-one-%d", index)), fake.Now())
		part2AddCaseBreak(t, world, caseOne, breakID)
	}

	caseTwo := part2InsertCase(t, world, 1234, "USD")
	part2AddCaseBreak(t, world, caseTwo,
		part2InsertBreak(t, world, scopeA, runA, "external_only", "USD",
			-1234, part2Signature("history-case-two"), fake.Now()))

	differentDelta := part2InsertCase(t, world, 999, "USD")
	part2AddCaseBreak(t, world, differentDelta,
		part2InsertBreak(t, world, scopeA, runA, "external_only", "USD",
			-999, part2Signature("history-different-delta"), fake.Now()))

	differentSide := part2InsertCase(t, world, 1234, "USD")
	part2AddCaseBreak(t, world, differentSide,
		part2InsertBreak(t, world, scopeA, runA, "ledger_only", "USD",
			-1234, part2Signature("history-different-side"), fake.Now()))

	differentScope := part2InsertCase(t, world, 1234, "USD")
	part2AddCaseBreak(t, world, differentScope,
		part2InsertBreak(t, world, scopeB, runB, "external_only", "USD",
			-1234, part2Signature("history-different-scope"), fake.Now()))

	differentCurrency := part2InsertCase(t, world, 1234, "USD")
	part2AddCaseBreak(t, world, differentCurrency,
		part2InsertBreak(t, world, scopeA, runA, "external_only", "EUR",
			-1234, part2Signature("history-different-currency"), fake.Now()))

	openCase := part2InsertCase(t, world, 1234, "USD")
	part2AddCaseBreak(t, world, openCase,
		part2InsertBreak(t, world, scopeA, runA, "external_only", "USD",
			-1234, part2Signature("history-open-case"), fake.Now()))

	part2ResolveCase(t, service, caseOne, "link")
	fake.Advance(time.Minute)
	part2ResolveCase(t, service, caseTwo, "write_off")
	fake.Advance(time.Minute)
	for _, caseID := range []string{differentDelta, differentSide, differentScope, differentCurrency} {
		part2ResolveCase(t, service, caseID, "link")
		fake.Advance(time.Minute)
	}
	// Resolve the current case last: it is structurally identical and would be
	// the first row if the current-case exclusion were missing.
	part2ResolveCase(t, service, world.CaseID, "adjustment")

	harness := newPart2Harness(t, world, fake)
	raw := harness.call(t, "get_history", fmt.Sprintf(`{"signature":%q}`, currentSignature))
	envelope := part2DecodeSuccess[part2HistoryData](t, raw)
	if envelope.Truncated {
		t.Fatal("two matching cases were truncated")
	}
	if envelope.Data.Signature != currentSignature {
		t.Fatalf("signature = %q, want %q", envelope.Data.Signature, currentSignature)
	}
	if envelope.Data.Match.ScopeID != scopeA || envelope.Data.Match.Side != "external_only" ||
		envelope.Data.Match.Currency != "USD" || envelope.Data.Match.DeltaMinor != -1234 {
		t.Fatalf("match = %+v", envelope.Data.Match)
	}
	if len(envelope.Data.Cases) != 2 {
		t.Fatalf("cases = %+v, want exactly two structurally similar cases", envelope.Data.Cases)
	}
	if envelope.Data.Cases[0].CaseID != caseTwo || envelope.Data.Cases[1].CaseID != caseOne {
		t.Fatalf("history order = %v, want resolved_at DESC (caseTwo then caseOne)",
			envelope.Data.Cases)
	}
	if envelope.Data.Cases[0].MatchingBreaks != 1 {
		t.Fatalf("caseTwo matching_breaks = %d, want 1", envelope.Data.Cases[0].MatchingBreaks)
	}
	if envelope.Data.Cases[1].MatchingBreaks != 2 {
		t.Fatalf("caseOne matching_breaks = %d, want 2", envelope.Data.Cases[1].MatchingBreaks)
	}
	if envelope.Data.Cases[0].ValueMinor != 1234 ||
		envelope.Data.Cases[0].Currency != "USD" ||
		envelope.Data.Cases[0].ResolutionType != "write_off" ||
		envelope.Data.Cases[0].ResolvedAt == "" {
		t.Fatalf("caseTwo = %+v", envelope.Data.Cases[0])
	}
	for _, result := range envelope.Data.Cases {
		if result.CaseID == world.CaseID {
			t.Fatal("current case was not excluded from history")
		}
		if result.CaseID == openCase || result.CaseID == differentDelta ||
			result.CaseID == differentSide || result.CaseID == differentScope ||
			result.CaseID == differentCurrency {
			t.Fatalf("non-matching case %s leaked into history", result.CaseID)
		}
	}
	if len(envelope.Refs) != 0 {
		t.Fatalf("history returned refs: %v", envelope.Refs)
	}

	// A known signature with no comparable resolved history is an empty list,
	// not not_found.
	noHistorySignature := part2Signature("history-none")
	part2InsertBreak(t, world, scopeB, runB, "external_only", "USD",
		-777, noHistorySignature, fake.Now())
	raw = harness.call(t, "get_history", fmt.Sprintf(`{"signature":%q}`, noHistorySignature))
	envelope = part2DecodeSuccess[part2HistoryData](t, raw)
	if len(envelope.Data.Cases) != 0 || envelope.Truncated {
		t.Fatalf("no-history result = %+v, want empty cases and no truncation", envelope)
	}
	if envelope.Data.Cases == nil {
		t.Fatal("cases is null, want []")
	}

	raw = harness.call(t, "get_history",
		`{"signature":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`)
	part2RequireErrorCode(t, raw, tools.CodeNotFound)
}

func TestIntegrationGetHistoryCap(t *testing.T) {
	fake := clock.NewFake(part2Time(t, "2026-02-01T00:00:00Z"))
	world := newIntegrationWorld(t)
	accountID := part2InsertAccount(t, world, "USD")
	scopeID := part2InsertScope(t, world, accountID, "psp_a", "USD")
	matchRunID := part2InsertMatchRun(t, world, scopeID)
	service := part2CaseService(t, world, fake)

	currentSignature := part2Signature("history-cap-current")
	currentBreak := part2InsertBreak(t, world, scopeID, matchRunID, "external_only", "USD",
		-4321, currentSignature, fake.Now())
	part2AddCaseBreak(t, world, world.CaseID, currentBreak)

	ordered := make([]string, 0, 21)
	for index := 0; index < 21; index++ {
		caseID := part2InsertCase(t, world, 4321, "USD")
		part2AddCaseBreak(t, world, caseID,
			part2InsertBreak(t, world, scopeID, matchRunID, "external_only", "USD",
				-4321, part2Signature(fmt.Sprintf("history-cap-%02d", index)), fake.Now()))
		part2ResolveCase(t, service, caseID, "link")
		ordered = append(ordered, caseID)
		fake.Advance(time.Minute)
	}
	part2ResolveCase(t, service, world.CaseID, "adjustment")

	harness := newPart2Harness(t, world, fake)
	raw := harness.call(t, "get_history", fmt.Sprintf(`{"signature":%q}`, currentSignature))
	envelope := part2DecodeSuccess[part2HistoryData](t, raw)
	if !envelope.Truncated {
		t.Fatal("21 matching cases were not marked truncated")
	}
	if len(envelope.Data.Cases) != 20 {
		t.Fatalf("cases = %d, want the 20-case cap", len(envelope.Data.Cases))
	}
	if envelope.Data.Cases[0].CaseID != ordered[20] {
		t.Fatalf("first case = %q, want the most recently resolved %q",
			envelope.Data.Cases[0].CaseID, ordered[20])
	}
	if envelope.Data.Cases[19].CaseID != ordered[1] {
		t.Fatalf("last case = %q, want %q (the oldest matching case is dropped)",
			envelope.Data.Cases[19].CaseID, ordered[1])
	}
	for _, result := range envelope.Data.Cases {
		if result.CaseID == world.CaseID || result.CaseID == ordered[0] {
			t.Fatalf("unexpected case %s in capped history", result.CaseID)
		}
	}
	if len(envelope.Refs) != 0 {
		t.Fatalf("history returned refs: %v", envelope.Refs)
	}
}
