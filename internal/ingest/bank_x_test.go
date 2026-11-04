package ingest_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/internal/money"
)

var _ ingest.Parser = ingest.NewBankParser()

const bankHeader = "record_id,booking_date,value_date,amount,currency,description,bank_reference\n"

// bankFile builds a bank_x file from a metadata body and raw data rows.
func bankFile(metadata, rows string) string {
	return "# " + metadata + "\n" + bankHeader + rows
}

const bankCreditRow = "b1,2026-06-15,2026-06-15,10.00,USD,deposit,BNK-1\n"

func parseBank(t *testing.T, input string) (ingest.ParseResult, error) {
	t.Helper()
	return ingest.NewBankParser().Parse(strings.NewReader(input))
}

// covers: FIN-03, FIN-40
func TestBankTypedErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []error
	}{
		{
			name: "missing required column",
			input: "# rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15\n" +
				"record_id,booking_date,value_date,amount,currency,bank_reference\n" + bankCreditRow,
			want: []error{ingest.ErrMissingColumn},
		},
		{
			name: "unexpected column",
			input: "# rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15\n" +
				"record_id,booking_date,value_date,amount,currency,description,bank_reference,extra\n" + bankCreditRow,
			want: []error{ingest.ErrUnknownColumn},
		},
		{
			name:  "malformed booking date",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", "b1,15/06/2026,2026-06-15,10.00,USD,deposit,BNK-1\n"),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "malformed value date",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", "b1,2026-06-15,2026-02-30,10.00,USD,deposit,BNK-1\n"),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "malformed statement date",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-13-01", bankCreditRow),
			want:  []error{ingest.ErrMalformedDate},
		},
		{
			name:  "wrong currency",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", "b1,2026-06-15,2026-06-15,10.00,EUR,deposit,BNK-1\n"),
			want:  []error{ingest.ErrCurrencyMismatch},
		},
		{
			name:  "missing opening balance",
			input: bankFile("rows=1 total=10.00 currency=USD closing=10.00 as_of=2026-06-15", bankCreditRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "malformed opening balance",
			input: bankFile("rows=1 total=10.00 currency=USD opening=1,000.00 closing=10.00 as_of=2026-06-15", bankCreditRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "duplicate metadata key",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 opening=0.00 closing=10.00 as_of=2026-06-15", bankCreditRow),
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "zero amount",
			input: bankFile("rows=1 total=0.00 currency=USD opening=0.00 closing=0.00 as_of=2026-06-15", "b1,2026-06-15,2026-06-15,0.00,USD,noop,BNK-1\n"),
			want:  []error{ingest.ErrInvalidSign},
		},
		{
			name:  "excess precision amount",
			input: bankFile("rows=1 total=-10 currency=JPY opening=0 closing=-10 as_of=2026-06-15", "b1,2026-06-15,2026-06-15,-10.5,JPY,fee,BNK-1\n"),
			want:  []error{ingest.ErrInvalidAmount, money.ErrExcessPrecision},
		},
		{
			name: "duplicate record id",
			input: bankFile("rows=2 total=20.00 currency=USD opening=0.00 closing=20.00 as_of=2026-06-15",
				bankCreditRow+"b1,2026-06-15,2026-06-15,10.00,USD,deposit,BNK-2\n"),
			want: []error{ingest.ErrDuplicateRecordID},
		},
		{
			name:  "empty file",
			input: "",
			want:  []error{ingest.ErrBadMetadata},
		},
		{
			name:  "metadata without header",
			input: "# rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15\n",
			want:  []error{ingest.ErrInvalidFormat},
		},
		{
			name:  "row field count",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", "b1,2026-06-15,2026-06-15,10.00,USD,deposit\n"),
			want:  []error{ingest.ErrMalformedRow},
		},
		{
			name:  "unquoted comma in description",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", "b1,2026-06-15,2026-06-15,10.00,USD,deposit, from psp,BNK-1\n"),
			want:  []error{ingest.ErrMalformedRow},
		},
		{
			name:  "invalid utf8",
			input: bankFile("rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15", bankCreditRow) + "\xff",
			want:  []error{ingest.ErrInvalidEncoding},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseBank(t, tc.input)
			if err == nil {
				t.Fatalf("Parse accepted the file and returned %+v", result)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("Parse error = %v, want errors.Is(_, %v)", err, want)
				}
			}
		})
	}
}

// covers: FIN-03, FIN-40
// TestBankRecordIDWhitespace pins the record_id contract: a value is rejected
// only when it is empty after trimming whitespace, and an accepted non-empty
// value is preserved verbatim (surrounding spaces are not rewritten).
func TestBankRecordIDWhitespace(t *testing.T) {
	metadata := "rows=1 total=10.00 currency=USD opening=0.00 closing=10.00 as_of=2026-06-15"
	rejected := []struct {
		name string
		id   string
	}{
		{name: "single space", id: " "},
		{name: "tab only", id: "\t"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			input := bankFile(metadata,
				tc.id+",2026-06-15,2026-06-15,10.00,USD,deposit,BNK-1\n")
			result, err := parseBank(t, input)
			if err == nil {
				t.Fatalf("Parse accepted record_id %q and returned %+v", tc.id, result)
			}
			if !errors.Is(err, ingest.ErrMalformedRow) {
				t.Fatalf("Parse error = %v, want errors.Is(_, ingest.ErrMalformedRow)", err)
			}
		})
	}

	t.Run("surrounding spaces preserved", func(t *testing.T) {
		const id = "  b1  "
		input := bankFile(metadata,
			id+",2026-06-15,2026-06-15,10.00,USD,deposit,BNK-1\n")
		result, err := parseBank(t, input)
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
func TestBankRecordMapping(t *testing.T) {
	data, err := readFixture("bank_x", "day.csv")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	result, err := parseBank(t, string(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := result.Declared.Currency, "USD"; got != want {
		t.Errorf("declared currency = %q, want %q", got, want)
	}
	if got, want := result.Declared.RowCount, 3; got != want {
		t.Errorf("declared row count = %d, want %d", got, want)
	}
	if got, want := result.Declared.TotalMinor, int64(8500); got != want {
		t.Errorf("declared total = %d, want %d", got, want)
	}
	if len(result.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(result.Records))
	}
	wantKinds := []ingest.Kind{ingest.KindPayout, ingest.KindFee, ingest.KindFee}
	wantAmounts := []int64{12000, -2500, -1000}
	wantValueDates := []string{"2026-06-15", "2026-06-16", "2026-06-15"}
	for index, record := range result.Records {
		if record.Kind != wantKinds[index] {
			t.Errorf("record %d kind = %q, want %q", index, record.Kind, wantKinds[index])
		}
		if record.AmountMinor != wantAmounts[index] {
			t.Errorf("record %d amount = %d, want %d", index, record.AmountMinor, wantAmounts[index])
		}
		if record.FeeMinor != 0 {
			t.Errorf("record %d fee = %d, want 0", index, record.FeeMinor)
		}
		if got := record.ValueDate.UTC().Format("2006-01-02"); got != wantValueDates[index] {
			t.Errorf("record %d value date = %s, want %s", index, got, wantValueDates[index])
		}
		if !record.OccurredAt.Equal(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("record %d occurred_at = %s, want booking date midnight UTC", index, record.OccurredAt)
		}
		if !json.Valid(record.Raw) {
			t.Errorf("record %d raw is not valid JSON: %q", index, record.Raw)
		}
	}
	var raw map[string]string
	if err := json.Unmarshal(result.Records[1].Raw, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if got, want := raw["description"], "wire fee, international"; got != want {
		t.Errorf("raw description = %q, want %q", got, want)
	}
	if got, want := raw["bank_reference"], "BNK-0002"; got != want {
		t.Errorf("raw bank_reference = %q, want %q", got, want)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("statements = %d, want 1", len(result.Statements))
	}
	statement := result.Statements[0]
	if got, want := statement.AsOfDate.UTC().Format("2006-01-02"), "2026-06-15"; got != want {
		t.Errorf("statement date = %s, want %s", got, want)
	}
	if got, want := statement.OpeningMinor, int64(100000); got != want {
		t.Errorf("statement opening = %d, want %d", got, want)
	}
	if got, want := statement.ClosingMinor, int64(108500); got != want {
		t.Errorf("statement closing = %d, want %d", got, want)
	}
	if got, want := statement.Currency, "USD"; got != want {
		t.Errorf("statement currency = %q, want %q", got, want)
	}
}

// covers: FIN-03
func TestBankJPYZeroDecimal(t *testing.T) {
	valid := bankFile("rows=1 total=-100 currency=JPY opening=1000 closing=900 as_of=2026-06-15",
		"b1,2026-06-15,2026-06-15,-100,JPY,atm fee,BNK-1\n")
	result, err := parseBank(t, valid)
	if err != nil {
		t.Fatalf("Parse JPY: %v", err)
	}
	if got, want := result.Records[0].AmountMinor, int64(-100); got != want {
		t.Fatalf("JPY amount = %d minor, want %d", got, want)
	}
	if got, want := result.Statements[0].OpeningMinor, int64(1000); got != want {
		t.Fatalf("JPY opening = %d minor, want %d", got, want)
	}
	if got, want := result.Statements[0].ClosingMinor, int64(900); got != want {
		t.Fatalf("JPY closing = %d minor, want %d", got, want)
	}

	fractional := bankFile("rows=1 total=-100 currency=JPY opening=1000 closing=900 as_of=2026-06-15",
		"b1,2026-06-15,2026-06-15,-100.5,JPY,atm fee,BNK-1\n")
	if _, err := parseBank(t, fractional); err == nil {
		t.Fatal("Parse accepted -100.5 JPY, want excess precision")
	} else if !errors.Is(err, money.ErrExcessPrecision) {
		t.Fatalf("Parse error = %v, want money.ErrExcessPrecision", err)
	}
}

// covers: FIN-03, FIN-40
func TestBankSourceAndUntrustedFields(t *testing.T) {
	parser := ingest.NewBankParser()
	if got, want := parser.Source(), ingest.SourceBank; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	fields := parser.UntrustedFields()
	if len(fields) != 1 || fields[0] != "description" {
		t.Errorf("UntrustedFields = %v, want [description]", fields)
	}
	fields[0] = "mutated"
	if got := parser.UntrustedFields()[0]; got != "description" {
		t.Errorf("UntrustedFields is not defensive: %q", got)
	}
}
