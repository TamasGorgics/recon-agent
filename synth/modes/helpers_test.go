package modes_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

const testNow = "2026-06-15T12:00:00Z"

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func testOptions(seed int64, days int, difficulty string) synth.Options {
	return synth.Options{
		Seed:       seed,
		Days:       days,
		Difficulty: difficulty,
		Config:     config.DefaultSynthConfig(),
	}
}

func generate(t *testing.T, seed int64, days int, difficulty string, cfg modes.Config) *modes.Result {
	t.Helper()
	result, err := modes.Generate(clock.NewFake(mustTime(t, testNow)),
		testOptions(seed, days, difficulty), cfg)
	if err != nil {
		t.Fatalf("Generate(seed=%d, days=%d, %s): %v", seed, days, difficulty, err)
	}
	return result
}

func singleMode(mode string, count int) modes.Config {
	return modes.Config{Total: count, Counts: map[string]int{mode: count}}
}

func scenariosOf(result *modes.Result, mode string) []synth.Scenario {
	var scenarios []synth.Scenario
	for _, scenario := range result.Truth.Scenarios {
		if scenario.Mode == mode {
			scenarios = append(scenarios, scenario)
		}
	}
	return scenarios
}

func txnIndex(result *modes.Result) map[string]synth.Transaction {
	index := make(map[string]synth.Transaction, len(result.Ledger.Transactions))
	for _, txn := range result.Ledger.Transactions {
		index[txn.ID] = txn
	}
	return index
}

func mustTxn(t *testing.T, index map[string]synth.Transaction, id string) synth.Transaction {
	t.Helper()
	txn, ok := index[id]
	if !ok {
		t.Fatalf("ledger txn %s not found", id)
	}
	return txn
}

// pspRecord is one record parsed back with the real psp_a parser, plus the
// reference the parser keeps in the untrusted raw payload.
type pspRecord struct {
	Path      string
	Record    ingest.Record
	Reference string
	BatchID   string
}

func parsePSPFiles(t *testing.T, result *modes.Result) map[string]pspRecord {
	t.Helper()
	parser := ingest.NewPSPParser()
	records := make(map[string]pspRecord)
	for _, file := range result.Files {
		if !strings.HasPrefix(file.Path, synth.SourcePSP+"/") {
			continue
		}
		parsed, err := parser.Parse(bytes.NewReader(file.Data))
		if err != nil {
			t.Fatalf("parse mutated %s: %v", file.Path, err)
		}
		if parsed.Declared.RowCount != len(parsed.Records) {
			t.Errorf("%s declares %d rows but parsed %d",
				file.Path, parsed.Declared.RowCount, len(parsed.Records))
		}
		var total int64
		for _, record := range parsed.Records {
			total += record.AmountMinor
		}
		if parsed.Declared.TotalMinor != total {
			t.Errorf("%s declares total %d but records sum %d",
				file.Path, parsed.Declared.TotalMinor, total)
		}
		for _, record := range parsed.Records {
			var raw struct {
				Reference string `json:"reference"`
				BatchID   string `json:"batch_id"`
			}
			if err := json.Unmarshal(record.Raw, &raw); err != nil {
				t.Fatalf("%s record %s raw: %v", file.Path, record.SourceRecordID, err)
			}
			records[record.SourceRecordID] = pspRecord{
				Path:      file.Path,
				Record:    record,
				Reference: raw.Reference,
				BatchID:   raw.BatchID,
			}
		}
	}
	return records
}

func clearingNet(t *testing.T, txn synth.Transaction) int64 {
	t.Helper()
	var net int64
	found := false
	for _, line := range txn.Lines {
		if !strings.HasPrefix(line.AccountCode, "clearing_") {
			continue
		}
		found = true
		if line.Direction == "debit" {
			net += line.AmountMinor
		} else {
			net -= line.AmountMinor
		}
	}
	if !found {
		t.Fatalf("txn %s has no clearing line", txn.ID)
	}
	return net
}

func debitOn(t *testing.T, txn synth.Transaction, accountCode string) int64 {
	t.Helper()
	for _, line := range txn.Lines {
		if line.AccountCode == accountCode && line.Direction == "debit" {
			return line.AmountMinor
		}
	}
	t.Fatalf("txn %s has no debit on %s", txn.ID, accountCode)
	return 0
}

func clearingLine(t *testing.T, txn synth.Transaction) synth.Line {
	t.Helper()
	for _, line := range txn.Lines {
		if strings.HasPrefix(line.AccountCode, "clearing_") && line.ExternalRef != "" {
			return line
		}
	}
	t.Fatalf("txn %s has no clearing line with an external reference", txn.ID)
	return synth.Line{}
}

func scheduleFor(t *testing.T, result *modes.Result, source, kind, currency string) synth.FeeSchedule {
	t.Helper()
	for _, schedule := range result.Ledger.FeeSchedules {
		if schedule.Source == source && schedule.Kind == kind && schedule.Currency == currency {
			return schedule
		}
	}
	t.Fatalf("no fee schedule for %s/%s/%s", source, kind, currency)
	return synth.FeeSchedule{}
}

// feeMinor mirrors the generator's integer fee formula. Test-only code.
func feeMinor(gross int64, pctBps int, fixedMinor int64) int64 {
	if gross <= 0 {
		return fixedMinor
	}
	return (gross*int64(pctBps)+5_000)/10_000 + fixedMinor
}

func assertFailureScenarioSchema(t *testing.T, scenario synth.Scenario) {
	t.Helper()
	if scenario.ScenarioID == "" {
		t.Error("scenario_id is empty")
	}
	if !isPartAMode(scenario.Mode) {
		t.Errorf("scenario %s mode = %q, want a part-A mode", scenario.ScenarioID, scenario.Mode)
	}
	if scenario.Difficulty == "" {
		t.Errorf("scenario %s has no difficulty", scenario.ScenarioID)
	}
	if len(scenario.BreakSignature) != 64 {
		t.Errorf("scenario %s break_signature = %q, want 64 hex bytes",
			scenario.ScenarioID, scenario.BreakSignature)
	} else if _, err := hex.DecodeString(scenario.BreakSignature); err != nil {
		t.Errorf("scenario %s break_signature is not hex: %v", scenario.ScenarioID, err)
	}
	if len(scenario.ExpectedResolutions) == 0 {
		t.Errorf("scenario %s has no expected_resolutions", scenario.ScenarioID)
	}
	primaryFound := false
	for _, resolution := range scenario.ExpectedResolutions {
		if resolution == scenario.PrimaryResolution {
			primaryFound = true
		}
	}
	if !primaryFound {
		t.Errorf("scenario %s primary_resolution %q is not in %v",
			scenario.ScenarioID, scenario.PrimaryResolution, scenario.ExpectedResolutions)
	}
	if scenario.ExpectedTier == "" {
		t.Errorf("scenario %s has no expected_tier", scenario.ScenarioID)
	}
	if len(scenario.Amounts) == 0 {
		t.Errorf("scenario %s has no amounts", scenario.ScenarioID)
	}
	for _, value := range scenario.Amounts {
		if len(value.Currency) != 3 {
			t.Errorf("scenario %s amount currency %q", scenario.ScenarioID, value.Currency)
		}
	}
	if len(scenario.Accounts) == 0 {
		t.Errorf("scenario %s has no accounts", scenario.ScenarioID)
	}
	if len(scenario.LedgerTxnIDs) == 0 {
		t.Errorf("scenario %s has no ledger_txn_ids", scenario.ScenarioID)
	}
	for _, ref := range scenario.ExternalRecords {
		if ref.Source == "" || ref.SourceRecordID == "" {
			t.Errorf("scenario %s has an empty external record ref: %+v", scenario.ScenarioID, ref)
		}
	}
	if scenario.Mode != modes.MissingSettlement && len(scenario.ExternalRecords) == 0 {
		t.Errorf("scenario %s (%s) has no external record", scenario.ScenarioID, scenario.Mode)
	}
}

func isPartAMode(mode string) bool {
	for _, name := range modes.PartAModes {
		if name == mode {
			return true
		}
	}
	return false
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func hashFiles(files []synth.File) map[string]string {
	hashes := make(map[string]string, len(files))
	for _, file := range files {
		sum := sha256.Sum256(file.Data)
		hashes[file.Path] = hex.EncodeToString(sum[:])
	}
	return hashes
}

func fileData(t *testing.T, result *modes.Result, path string) []byte {
	t.Helper()
	for _, file := range result.Files {
		if file.Path == path {
			return file.Data
		}
	}
	t.Fatalf("output %q not found", path)
	return nil
}

func sortedPaths(files []synth.File) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return paths
}

func cutoffFor(t *testing.T, source string, at time.Time) time.Time {
	t.Helper()
	sourceConfig, ok := config.DefaultSynthIngestSources()[source]
	if !ok {
		t.Fatalf("no source config for %s", source)
	}
	location, err := time.LoadLocation(sourceConfig.Timezone)
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	parsed, err := time.Parse("15:04", sourceConfig.Cutoff)
	if err != nil {
		t.Fatalf("parse cutoff: %v", err)
	}
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), parsed.Hour(), parsed.Minute(),
		0, 0, location).UTC()
}
