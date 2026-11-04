package ingest_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

var _ ingest.Parser = ingest.NewPSPParser()

const pspHeader = "record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\n"

// pspFile builds a psp_a file from a metadata body and raw data rows.
func pspFile(metadata, rows string) string {
	return "# " + metadata + "\n" + pspHeader + rows
}

const pspChargeRow = "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n"

func parsePSP(t *testing.T, input string) (ingest.ParseResult, error) {
	t.Helper()
	return ingest.NewPSPParser().Parse(strings.NewReader(input))
}

// covers: FIN-03, FIN-40
func TestPSPTypedErrors(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		want     []error
		contains string
	}{
		{
			name:  "unknown record type",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,credit,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrUnknownRecordType},
		},
		{
			name: "missing required column",
			input: "# rows=1 total=10.00 currency=USD batch=B1\n" +
				"record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id\n" + pspChargeRow,
			want: []error{ingest.ErrMissingColumn},
		},
		{
			name: "unexpected column",
			input: "# rows=1 total=10.00 currency=USD batch=B1\n" +
				"record_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo,extra\n" + pspChargeRow,
			want: []error{ingest.ErrUnknownColumn},
		},
		{
			name:  "malformed timestamp",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-13-40T99:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "malformed settlement timestamp",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,not-a-time,10.00,0.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "zero timestamp",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,0001-01-01T00:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "wrong currency",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,EUR,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrCurrencyMismatch},
		},
		{
			name:  "missing total metadata",
			input: pspFile("rows=1 currency=USD batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "unknown metadata key",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1 extra=1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "duplicate metadata key",
			input: pspFile("rows=1 rows=1 total=10.00 currency=USD batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "non-integer row count",
			input: pspFile("rows=one total=10.00 currency=USD batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "negative row count",
			input: pspFile("rows=-1 total=10.00 currency=USD batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "unknown metadata currency",
			input: pspFile("rows=1 total=10.00 currency=ZZZ batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata, money.ErrUnknownCurrency},
		},
		{
			name:  "metadata total excess precision",
			input: pspFile("rows=1 total=10.001 currency=USD batch=B1", pspChargeRow),
			want:  []error{ingest.ErrBadMetadata, money.ErrExcessPrecision},
		},
		{
			name:  "metadata line without hash",
			input: "rows=1 total=10.00 currency=USD batch=B1\n" + pspHeader + pspChargeRow,
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "empty file",
			input: "",
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "metadata without header",
			input: "# rows=1 total=10.00 currency=USD batch=B1\n",
			want:  []error{ingest.ErrInvalidFormat},
		},
		{
			name:  "excess precision amount",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.001,0.00,10.001,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrInvalidAmount, money.ErrExcessPrecision},
		},
		{
			name:  "over-long amount",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,"+strings.Repeat("9", 64)+",0.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrInvalidAmount},
		},
		{
			name:  "malformed decimal",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,ten,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrInvalidAmount, money.ErrInvalidAmount},
		},
		{
			name:  "zero net",
			input: pspFile("rows=1 total=0.00 currency=USD batch=B1", "r1,fee,2026-06-15T10:00:00Z,,0.00,0.00,0.00,USD,,B1,memo\n"),
			want:  []error{ingest.ErrInvalidSign},
		},
		{
			name:  "wrong sign for charge",
			input: pspFile("rows=1 total=-10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,-10.00,0.00,-10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrInvalidSign},
		},
		{
			name:  "net differs from gross minus fee",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,1.00,10.00,USD,REF-1,B1,memo\n"),
			want:  []error{ingest.ErrAmountMismatch},
		},
		{
			name:  "batch id mismatch",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B2,memo\n"),
			want:  []error{ingest.ErrMetadataMismatch},
		},
		{
			name: "duplicate record id",
			input: pspFile("rows=2 total=20.00 currency=USD batch=B1",
				pspChargeRow+
					"r1,charge,2026-06-15T11:00:00Z,,10.00,0.00,10.00,USD,REF-2,B1,memo\n"),
			want: []error{ingest.ErrDuplicateRecordID},
		},
		{
			name:  "invalid utf8",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", pspChargeRow) + "\xff",
			want:  []error{ingest.ErrInvalidEncoding},
		},
		{
			name:  "row field count",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1\n"),
			want:  []error{ingest.ErrMalformedRow},
		},
		{
			name:  "unquoted comma in memo",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1", "r1,charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,widget, blue\n"),
			want:  []error{ingest.ErrMalformedRow},
		},
		{
			name:  "malformed period date",
			input: pspFile("rows=1 total=10.00 currency=USD batch=B1 period_start=2026-02-30 period_end=2026-03-01", pspChargeRow),
			want:  []error{ingest.ErrMalformedDate},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parsePSP(t, tc.input)
			if err == nil {
				t.Fatalf("Parse accepted the file and returned %+v", result)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("Parse error = %v, want errors.Is(_, %v)", err, want)
				}
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, tc.contains)
			}
		})
	}
}

// covers: FIN-03, FIN-40
// TestPSPRecordIDWhitespace pins the record_id contract: a value is rejected
// only when it is empty after trimming whitespace, and an accepted non-empty
// value is preserved verbatim (surrounding spaces are not rewritten).
func TestPSPRecordIDWhitespace(t *testing.T) {
	metadata := "rows=1 total=10.00 currency=USD batch=B1"
	rejected := []struct {
		name string
		id   string
	}{
		{name: "single space", id: " "},
		{name: "tab only", id: "\t"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			input := pspFile(metadata,
				tc.id+",charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n")
			result, err := parsePSP(t, input)
			if err == nil {
				t.Fatalf("Parse accepted record_id %q and returned %+v", tc.id, result)
			}
			if !errors.Is(err, ingest.ErrMalformedRow) {
				t.Fatalf("Parse error = %v, want errors.Is(_, ingest.ErrMalformedRow)", err)
			}
		})
	}

	t.Run("surrounding spaces preserved", func(t *testing.T) {
		const id = "  r1  "
		input := pspFile(metadata,
			id+",charge,2026-06-15T10:00:00Z,,10.00,0.00,10.00,USD,REF-1,B1,memo\n")
		result, err := parsePSP(t, input)
		if err != nil {
			t.Fatalf("Parse rejected record_id %q: %v", id, err)
		}
		if got := result.Records[0].SourceRecordID; got != id {
			t.Fatalf("SourceRecordID = %q, want %q (preserved verbatim)", got, id)
		}
		var raw map[string]string
		if err := json.Unmarshal(result.Records[0].Raw, &raw); err != nil {
			t.Fatalf("unmarshal raw: %v", err)
		}
		if got := raw["record_id"]; got != id {
			t.Fatalf("raw record_id = %q, want %q (preserved verbatim)", got, id)
		}
	})
}

// covers: FIN-03, FIN-40
func TestPSPRecordMapping(t *testing.T) {
	data, err := readFixture("psp_a", "all_types.csv")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	result, err := parsePSP(t, string(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := result.Declared.Currency, "USD"; got != want {
		t.Errorf("declared currency = %q, want %q", got, want)
	}
	if got, want := result.Declared.RowCount, 5; got != want {
		t.Errorf("declared row count = %d, want %d", got, want)
	}
	if got, want := result.Declared.TotalMinor, int64(-14020); got != want {
		t.Errorf("declared total = %d, want %d", got, want)
	}
	if len(result.Records) != 5 {
		t.Fatalf("records = %d, want 5", len(result.Records))
	}
	wantKinds := []ingest.Kind{
		ingest.KindCharge, ingest.KindRefund, ingest.KindFee, ingest.KindPayout, ingest.KindChargeback,
	}
	wantAmounts := []int64{9710, -3000, -125, -20000, -605}
	wantFees := []int64{290, 0, 125, 0, 0}
	for index, record := range result.Records {
		if record.Kind != wantKinds[index] {
			t.Errorf("record %d kind = %q, want %q", index, record.Kind, wantKinds[index])
		}
		if record.AmountMinor != wantAmounts[index] {
			t.Errorf("record %d amount = %d, want %d", index, record.AmountMinor, wantAmounts[index])
		}
		if record.FeeMinor != wantFees[index] {
			t.Errorf("record %d fee = %d, want %d", index, record.FeeMinor, wantFees[index])
		}
		if record.Currency != "USD" {
			t.Errorf("record %d currency = %q, want USD", index, record.Currency)
		}
		if !record.ValueDate.IsZero() {
			t.Errorf("record %d value date = %s, want zero (psp_a derives it)", index, record.ValueDate)
		}
		if !json.Valid(record.Raw) {
			t.Errorf("record %d raw is not valid JSON: %q", index, record.Raw)
		}
	}
	if result.Records[1].SettledAt != nil {
		t.Errorf("refund settled_at = %s, want nil", result.Records[1].SettledAt)
	}
	if result.Records[0].SettledAt == nil {
		t.Error("charge settled_at is nil, want the file timestamp")
	}
	if got, want := result.Records[0].OccurredAt, time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("charge occurred_at = %s, want %s", got, want)
	}
	var raw map[string]string
	if err := json.Unmarshal(result.Records[0].Raw, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if got, want := raw["memo"], "widget, blue"; got != want {
		t.Errorf("raw memo = %q, want %q", got, want)
	}
	if got, want := raw["batch_id"], "B-2026-06-15-001"; got != want {
		t.Errorf("raw batch_id = %q, want %q", got, want)
	}
	if len(result.Statements) != 0 {
		t.Errorf("psp_a statements = %d, want 0", len(result.Statements))
	}
	if got, want := result.PeriodStart, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("period start = %s, want %s", got, want)
	}
	if got, want := result.PeriodEnd, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("period end = %s, want %s", got, want)
	}
}

// covers: FIN-03
func TestPSPJPYZeroDecimal(t *testing.T) {
	valid := pspFile("rows=1 total=100 currency=JPY batch=B-JPY",
		"r1,charge,2026-06-15T10:00:00Z,,100,0,100,JPY,REF-JPY,B-JPY,\u5186\n")
	result, err := parsePSP(t, valid)
	if err != nil {
		t.Fatalf("Parse JPY: %v", err)
	}
	if got, want := result.Records[0].AmountMinor, int64(100); got != want {
		t.Fatalf("JPY amount = %d minor, want %d", got, want)
	}
	if got, want := result.Declared.TotalMinor, int64(100); got != want {
		t.Fatalf("JPY declared total = %d minor, want %d", got, want)
	}

	fractional := pspFile("rows=1 total=100 currency=JPY batch=B-JPY",
		"r1,charge,2026-06-15T10:00:00Z,,100.5,0,100.5,JPY,REF-JPY,B-JPY,\n")
	if _, err := parsePSP(t, fractional); err == nil {
		t.Fatal("Parse accepted 100.5 JPY, want excess precision")
	} else if !errors.Is(err, money.ErrExcessPrecision) {
		t.Fatalf("Parse error = %v, want money.ErrExcessPrecision", err)
	}
}

// covers: FIN-03, FIN-40
func TestPSPSourceAndUntrustedFields(t *testing.T) {
	parser := ingest.NewPSPParser()
	if got, want := parser.Source(), ingest.SourcePSP; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	fields := parser.UntrustedFields()
	if len(fields) != 1 || fields[0] != "memo" {
		t.Errorf("UntrustedFields = %v, want [memo]", fields)
	}
	fields[0] = "mutated"
	if got := parser.UntrustedFields()[0]; got != "memo" {
		t.Errorf("UntrustedFields is not defensive: %q", got)
	}
}

// readFixture reads one testdata fixture for a source.
func readFixture(source, name string) ([]byte, error) {
	return os.ReadFile("testdata/" + source + "/" + name)
}
