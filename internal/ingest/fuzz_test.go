package ingest_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// adversarialSeeds returns fragments that exercise the delimiter, quoting,
// metadata, and numeric boundaries of both formats. They are added to the fuzz
// corpus so every `go test` run parses them once.
func adversarialSeeds() []string {
	return []string{
		"",
		"\n\n",
		"\xef\xbb\xbf",
		"\xef\xbb\xbf# rows=0 total=0 currency=USD\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\n",
		"#",
		"# rows=1",
		"# rows=-1 total=0 currency=USD batch=B\n",
		"# rows=1 total=NaN currency=USD batch=B\n",
		"# rows=999999999999999999999999 total=0 currency=USD batch=B\n",
		"# rows=1 total=1 currency=XXX batch=B\n",
		"# rows=1 total=1 currency=USD batch=B\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\n",
		"# rows=1 total=1 currency=USD batch=B\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\nr1,charge,2026-06-15T10:00:00Z,,1,0,1,USD,R,B,m\n",
		"# rows=1 total=1 currency=USD batch=B\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\nr1,charge,0001-01-01T00:00:00Z,,1,0,1,USD,R,B,\"memo\nwith newline\"\n",
		"# rows=1 total=1 currency=USD batch=B\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\nr1,unknown,2026-06-15T10:00:00Z,,1,0,1,USD,R,B,\"unterminated\n",
		"# rows=1 total=1 currency=USD batch=B\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\nr1,charge,2026-06-15T10:00:00Z,,999999999999999999999999999999.99,0,999999999999999999999999999999.99,USD,R,B,\"\x00\x01\x02\"\n",
		"\xff\xfe\x00# rows=1 total=1 currency=USD batch=B\n",
		"# rows=1 total=1 currency=USD batch=B\n\nrecord_id,type,occurred_at,settled_at,gross,fee,net,currency,reference,batch_id,memo\n\r\nr1,charge,2026-06-15T10:00:00Z,,-1,-0,-1,USD,R,B,x\r\n",
		"# rows=1 total=0 currency=JPY opening=0 closing=0 as_of=2026-06-15\nrecord_id,booking_date,value_date,amount,currency,description,bank_reference\nb1,2026-06-15,2026-06-16,0,JPY,\"d, \"\"q\"\"\",\n",
		"# rows=1 total=1 currency=USD opening=0 closing=1 as_of=2026-02-30\nrecord_id,booking_date,value_date,amount,currency,description,bank_reference\nb1,2026-06-15,2026-06-16,1,USD,\"d, \"\"q\"\"\",\n",
		"# rows=1 total=1 currency=USD opening=0 closing=1 as_of=2026-06-15\nextra\n",
		strings.Repeat("a", 4096),
	}
}

func seedCorpus(f *testing.F, dir string) {
	f.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		f.Fatalf("glob seeds: %v", err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("read seed %s: %v", path, err)
		}
		f.Add(data)
	}
	for _, seed := range adversarialSeeds() {
		f.Add([]byte(seed))
	}
}

// covers: FIN-03, FIN-40
func FuzzParsePSP(f *testing.F) {
	seedCorpus(f, filepath.Join("testdata", "psp_a"))
	parser := ingest.NewPSPParser()
	f.Fuzz(func(t *testing.T, data []byte) {
		assertParseSafe(t, parser, data)
	})
}

// covers: FIN-03, FIN-40
func FuzzParseBank(f *testing.F) {
	seedCorpus(f, filepath.Join("testdata", "bank_x"))
	parser := ingest.NewBankParser()
	f.Fuzz(func(t *testing.T, data []byte) {
		assertParseSafe(t, parser, data)
	})
}

// assertParseSafe checks the parser contract for accepted inputs. Rejected
// input is a normal outcome; the fuzz targets only require that no input
// panics, exhausts memory, or yields an inconsistent canonical record.
func assertParseSafe(t *testing.T, parser ingest.Parser, data []byte) {
	t.Helper()
	result, err := parser.Parse(bytes.NewReader(data))
	if err != nil {
		return
	}
	if strings.TrimSpace(result.Declared.Currency) == "" {
		t.Fatalf("accepted input has no declared currency: %q", data)
	}
	if result.Declared.RowCount < 0 {
		t.Fatalf("accepted input has negative declared row count %d", result.Declared.RowCount)
	}
	for index, record := range result.Records {
		if strings.TrimSpace(record.SourceRecordID) == "" {
			t.Fatalf("record %d has an empty source record id", index)
		}
		if !record.Kind.Valid() {
			t.Fatalf("record %d has invalid kind %q", index, record.Kind)
		}
		if record.OccurredAt.IsZero() {
			t.Fatalf("record %d has no occurred_at", index)
		}
		if record.FeeMinor < 0 {
			t.Fatalf("record %d has negative fee %d", index, record.FeeMinor)
		}
		if !json.Valid(record.Raw) {
			t.Fatalf("record %d raw payload is not valid JSON: %q", index, record.Raw)
		}
	}
	for index, statement := range result.Statements {
		if statement.AsOfDate.IsZero() {
			t.Fatalf("statement %d has no as-of date", index)
		}
		if strings.TrimSpace(statement.Currency) == "" {
			t.Fatalf("statement %d has no currency", index)
		}
	}
	if !result.PeriodStart.IsZero() && result.PeriodEnd.IsZero() {
		t.Fatalf("period_start is set but period_end is zero")
	}
	if result.PeriodStart.IsZero() && !result.PeriodEnd.IsZero() {
		t.Fatalf("period_end is set but period_start is zero")
	}
	if !result.PeriodStart.IsZero() && result.PeriodEnd.Before(result.PeriodStart) {
		t.Fatalf("period_end %s is before period_start %s", result.PeriodEnd, result.PeriodStart)
	}
}
