package ingest_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/ingest"
)

// update rewrites the golden files from the current parser output. It is only
// for intentional format changes; a normal run must match the committed files.
var update = flag.Bool("update", false, "rewrite golden files from the current parser output")

// covers: FIN-03, FIN-40
func TestGoldenPSP(t *testing.T) {
	runGolden(t, "psp_a", ingest.NewPSPParser())
}

// covers: FIN-03, FIN-40
func TestGoldenBank(t *testing.T) {
	runGolden(t, "bank_x", ingest.NewBankParser())
}

// goldenRecord is the stable, human-reviewable projection of one canonical
// record. Golden files deliberately do not depend on Go struct field names, so
// refactoring the Record type does not churn the fixtures.
type goldenRecord struct {
	SourceRecordID string          `json:"source_record_id"`
	Kind           string          `json:"kind"`
	AmountMinor    int64           `json:"amount_minor"`
	Currency       string          `json:"currency"`
	FeeMinor       int64           `json:"fee_minor"`
	OccurredAt     string          `json:"occurred_at"`
	SettledAt      *string         `json:"settled_at"`
	ValueDate      string          `json:"value_date,omitempty"`
	Raw            json.RawMessage `json:"raw"`
}

type goldenStatement struct {
	AsOfDate     string `json:"as_of_date"`
	OpeningMinor int64  `json:"opening_minor"`
	ClosingMinor int64  `json:"closing_minor"`
	Currency     string `json:"currency"`
}

type goldenResult struct {
	Declared    goldenDeclared    `json:"declared"`
	Records     []goldenRecord    `json:"records"`
	Statements  []goldenStatement `json:"statements,omitempty"`
	PeriodStart string            `json:"period_start,omitempty"`
	PeriodEnd   string            `json:"period_end,omitempty"`
}

type goldenDeclared struct {
	TotalMinor int64  `json:"total_minor"`
	RowCount   int    `json:"row_count"`
	Currency   string `json:"currency"`
}

// runGolden parses every testdata/<source>/*.csv fixture and compares the
// normalized result with its committed .golden file. -update rewrites them.
func runGolden(t *testing.T, source string, parser ingest.Parser) {
	t.Helper()
	inputs, err := filepath.Glob(filepath.Join("testdata", source, "*.csv"))
	if err != nil {
		t.Fatalf("glob %s fixtures: %v", source, err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no %s fixtures found", source)
	}
	for _, input := range inputs {
		name := strings.TrimSuffix(filepath.Base(input), ".csv")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(input)
			if err != nil {
				t.Fatalf("read %s: %v", input, err)
			}
			result, err := parser.Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("Parse(%s): %v", input, err)
			}
			got, err := json.MarshalIndent(normalizeResult(t, result), "", "  ")
			if err != nil {
				t.Fatalf("marshal golden result: %v", err)
			}
			got = append(got, '\n')

			goldenPath := strings.TrimSuffix(input, ".csv") + ".golden"
			if *update {
				if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
					t.Fatalf("write %s: %v", goldenPath, err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read %s (run: go test -run TestGolden -update ./internal/ingest): %v", goldenPath, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s\nrun: go test -run TestGolden -update ./internal/ingest",
					goldenPath, got, want)
			}
		})
	}
}

// normalizeResult converts the parser output into the deterministic golden
// projection: UTC timestamps at second precision, sorted raw keys so map order
// cannot leak into the fixture.
func normalizeResult(t *testing.T, result ingest.ParseResult) goldenResult {
	t.Helper()
	out := goldenResult{
		Declared: goldenDeclared{
			TotalMinor: result.Declared.TotalMinor,
			RowCount:   result.Declared.RowCount,
			Currency:   result.Declared.Currency,
		},
		Records: make([]goldenRecord, 0, len(result.Records)),
	}
	if !result.PeriodStart.IsZero() {
		out.PeriodStart = result.PeriodStart.UTC().Format("2006-01-02")
	}
	if !result.PeriodEnd.IsZero() {
		out.PeriodEnd = result.PeriodEnd.UTC().Format("2006-01-02")
	}
	for _, record := range result.Records {
		item := goldenRecord{
			SourceRecordID: record.SourceRecordID,
			Kind:           string(record.Kind),
			AmountMinor:    record.AmountMinor,
			Currency:       record.Currency,
			FeeMinor:       record.FeeMinor,
			OccurredAt:     record.OccurredAt.UTC().Format(time.RFC3339),
		}
		if record.SettledAt != nil {
			settled := record.SettledAt.UTC().Format(time.RFC3339)
			item.SettledAt = &settled
		}
		if !record.ValueDate.IsZero() {
			item.ValueDate = record.ValueDate.UTC().Format("2006-01-02")
		}
		item.Raw = sortedRaw(t, record.Raw)
		out.Records = append(out.Records, item)
	}
	for _, statement := range result.Statements {
		out.Statements = append(out.Statements, goldenStatement{
			AsOfDate:     statement.AsOfDate.UTC().Format("2006-01-02"),
			OpeningMinor: statement.OpeningMinor,
			ClosingMinor: statement.ClosingMinor,
			Currency:     statement.Currency,
		})
	}
	return out
}

// sortedRaw re-encodes the raw evidence object so its key order is stable.
func sortedRaw(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("record has no raw payload")
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("raw payload is not valid JSON: %v", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode raw payload: %v", err)
	}
	return encoded
}

