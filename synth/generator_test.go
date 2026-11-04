package synth_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/ingest"
	"github.com/TamasGorgics/recon-agent/synth"
)

const testNow = "2026-06-15T12:00:00Z"

func testOptions(seed int64, days int) synth.Options {
	return synth.Options{
		Seed:       seed,
		Days:       days,
		Difficulty: synth.DifficultyEasy,
		Config:     config.DefaultSynthConfig(),
	}
}

func generate(t *testing.T, opts synth.Options) *synth.Dataset {
	t.Helper()
	clk := clock.NewFake(mustTime(t, testNow))
	dataset, err := synth.New(clk).Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return dataset
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

func fileByPath(t *testing.T, dataset *synth.Dataset, path string) []byte {
	t.Helper()
	for _, file := range dataset.Files {
		if file.Path == path {
			return file.Data
		}
	}
	t.Fatalf("output %q not found; files: %v", path, filePaths(dataset))
	return nil
}

func filePaths(dataset *synth.Dataset) []string {
	paths := make([]string, 0, len(dataset.Files))
	for _, file := range dataset.Files {
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return paths
}

func hashFiles(files []synth.File) map[string]string {
	hashes := make(map[string]string, len(files))
	for _, file := range files {
		sum := sha256.Sum256(file.Data)
		hashes[file.Path] = hex.EncodeToString(sum[:])
	}
	return hashes
}

func decodeJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

// covers: FIN-06
func TestManifestAndCounts(t *testing.T) {
	dataset := generate(t, testOptions(1, 3))
	manifest := dataset.Manifest

	if manifest.GeneratorVersion != synth.GeneratorVersion {
		t.Errorf("generator_version = %q, want %q", manifest.GeneratorVersion, synth.GeneratorVersion)
	}
	if manifest.Seed != 1 || manifest.Days != 3 || manifest.Difficulty != synth.DifficultyEasy {
		t.Errorf("manifest identity = %+v", manifest)
	}
	if len(manifest.ConfigHash) != 64 {
		t.Errorf("config_hash = %q, want 64 hex characters", manifest.ConfigHash)
	}
	wantVersion := hashText(fmt.Sprintf("%s|%d|%s", manifest.GeneratorVersion, manifest.Seed, manifest.ConfigHash))
	if manifest.DatasetVersion != wantVersion {
		t.Errorf("dataset_version = %q, want sha256(version|seed|config_hash) = %q",
			manifest.DatasetVersion, wantVersion)
	}
	if manifest.DatasetVersion == manifest.ConfigHash {
		t.Error("dataset_version must not equal config_hash")
	}

	counts := manifest.Counts
	if counts.Accounts != len(dataset.Ledger.Accounts) || counts.Accounts == 0 {
		t.Errorf("accounts = %d, dataset has %d", counts.Accounts, len(dataset.Ledger.Accounts))
	}
	if counts.FeeSchedules != len(dataset.Ledger.FeeSchedules) || counts.FeeSchedules == 0 {
		t.Errorf("fee_schedules = %d, dataset has %d", counts.FeeSchedules, len(dataset.Ledger.FeeSchedules))
	}
	if counts.LedgerTxns != len(dataset.Ledger.Transactions) {
		t.Errorf("ledger_txns = %d, dataset has %d", counts.LedgerTxns, len(dataset.Ledger.Transactions))
	}
	if counts.ExternalRecords != len(dataset.Truth.Scenarios) {
		t.Errorf("external_records = %d, scenarios = %d", counts.ExternalRecords, len(dataset.Truth.Scenarios))
	}
	if counts.LedgerTxns != counts.ExternalRecords || counts.ExternalRecords != counts.GroundTruthLinks {
		t.Errorf("counts do not agree: %+v", counts)
	}

	required := []string{"manifest.json", "ledger.json", "ground_truth.json"}
	for _, name := range required {
		fileByPath(t, dataset, name)
	}
	pspFiles, bankFiles := 0, 0
	for _, file := range dataset.Files {
		switch {
		case strings.HasPrefix(file.Path, "psp_a/"):
			pspFiles++
		case strings.HasPrefix(file.Path, "bank_x/"):
			bankFiles++
		}
	}
	if pspFiles != 3*len(config.DefaultSynthConfig().Currencies) {
		t.Errorf("psp files = %d, want %d", pspFiles, 3*len(config.DefaultSynthConfig().Currencies))
	}
	if bankFiles != pspFiles {
		t.Errorf("bank files = %d, want %d", bankFiles, pspFiles)
	}
}

// covers: FIN-05
func TestNoDatabaseUUIDsAndExplicitTimestamps(t *testing.T) {
	dataset := generate(t, testOptions(9, 2))

	for _, transaction := range dataset.Ledger.Transactions {
		if got := transaction.ID[14]; got != '5' {
			t.Errorf("transaction %s is not a UUIDv5 (version nibble %c)", transaction.ID, got)
		}
		if got := transaction.ID[19]; got != '8' && got != '9' && got != 'a' && got != 'b' {
			t.Errorf("transaction %s has a non-RFC variant nibble %c", transaction.ID, got)
		}
		if transaction.CreatedAt == "" || transaction.BookedAt == "" || transaction.ValueDate == "" {
			t.Errorf("transaction %s has an empty timestamp", transaction.ID)
		}
		for field, value := range map[string]string{
			"created_at": transaction.CreatedAt,
			"booked_at":  transaction.BookedAt,
		} {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				t.Errorf("%s %q is not RFC 3339: %v", field, value, err)
				continue
			}
			if !strings.HasSuffix(value, "Z") || parsed.Location() != time.UTC {
				t.Errorf("%s %q is not UTC", field, value)
			}
		}
		if _, err := time.Parse("2006-01-02", transaction.ValueDate); err != nil {
			t.Errorf("value_date %q is not YYYY-MM-DD: %v", transaction.ValueDate, err)
		}
		for _, line := range transaction.Lines {
			if line.AccountID[14] != '5' {
				t.Errorf("account id %s is not a UUIDv5", line.AccountID)
			}
		}
	}
	for _, account := range dataset.Ledger.Accounts {
		if account.ID[14] != '5' {
			t.Errorf("account %s id %s is not a UUIDv5", account.Code, account.ID)
		}
	}
	for _, schedule := range dataset.Ledger.FeeSchedules {
		if schedule.ID[14] != '5' {
			t.Errorf("fee schedule %s id %s is not a UUIDv5", schedule.ID, schedule.ID)
		}
	}
}

// covers: FIN-05, FIN-06
func TestMinorUnitsAndFeeMath(t *testing.T) {
	cfg := config.DefaultSynthConfig()
	dataset := generate(t, synth.Options{
		Seed:       5,
		Days:       3,
		Difficulty: synth.DifficultyRealistic,
		Config:     cfg,
	})

	// The serialized JSON uses integer minor units: no decimal point in an
	// amount_minor value and no float in ground truth.
	var ledgerRaw map[string]any
	decodeJSON(t, fileByPath(t, dataset, "ledger.json"), &ledgerRaw)
	assertIntegerMinorAmounts(t, ledgerRaw, "transactions")
	var truthRaw map[string]any
	decodeJSON(t, fileByPath(t, dataset, "ground_truth.json"), &truthRaw)
	assertIntegerMinorAmounts(t, truthRaw, "scenarios")

	// Every psp_a charge's fee is exactly the schedule's integer formula.
	schedules := make(map[string]synth.FeeSchedule)
	for _, schedule := range dataset.Ledger.FeeSchedules {
		schedules[schedule.Source+"/"+schedule.Kind+"/"+schedule.Currency] = schedule
	}
	charges := 0
	for _, file := range dataset.Files {
		if !strings.HasPrefix(file.Path, "psp_a/") {
			continue
		}
		parsed, err := ingest.NewPSPParser().Parse(bytes.NewReader(file.Data))
		if err != nil {
			t.Fatalf("parse %s: %v", file.Path, err)
		}
		for _, record := range parsed.Records {
			if string(record.Kind) != "charge" {
				continue
			}
			charges++
			schedule, ok := schedules["psp_a/charge/"+record.Currency]
			if !ok {
				t.Fatalf("no charge schedule for %s", record.Currency)
			}
			wantFee := (record.AmountMinor + record.FeeMinor) * int64(schedule.PctBps)
			wantFee = (wantFee + 5_000) / 10_000
			wantFee += schedule.FixedMinor
			if record.FeeMinor != wantFee {
				t.Errorf("%s fee = %d, want %d from %dbps+%d",
					record.SourceRecordID, record.FeeMinor, wantFee, schedule.PctBps, schedule.FixedMinor)
			}
			// The parser validated gross-fee=net; recompute here to pin the
			// generator's fee schedule independent of the parser.
			gross := record.AmountMinor + record.FeeMinor
			if got := gross - record.FeeMinor; got != record.AmountMinor {
				t.Errorf("%s net = %d, want %d", record.SourceRecordID, record.AmountMinor, got)
			}
		}
	}
	if charges == 0 {
		t.Fatal("generated no psp_a charges")
	}
}

func assertIntegerMinorAmounts(t *testing.T, value any, section string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "amount_minor" {
				number, ok := child.(json.Number)
				if !ok {
					t.Errorf("%s: amount_minor is %T, want JSON number", section, child)
					continue
				}
				if strings.Contains(number.String(), ".") {
					t.Errorf("%s: amount_minor %s is not an integer", section, number.String())
				}
				if _, err := number.Int64(); err != nil {
					t.Errorf("%s: amount_minor %s does not fit int64: %v", section, number.String(), err)
				}
				continue
			}
			assertIntegerMinorAmounts(t, child, section)
		}
	case []any:
		for _, child := range typed {
			assertIntegerMinorAmounts(t, child, section)
		}
	}
}

func TestGroundTruthShapeAndOneLinkPerRecord(t *testing.T) {
	dataset := generate(t, testOptions(3, 4))
	if dataset.Truth.DatasetVersion != dataset.Manifest.DatasetVersion {
		t.Errorf("ground truth dataset_version = %q, want %q",
			dataset.Truth.DatasetVersion, dataset.Manifest.DatasetVersion)
	}

	type key struct{ source, id string }
	links := make(map[key]int)
	for _, scenario := range dataset.Truth.Scenarios {
		if scenario.ScenarioID == "" {
			t.Error("scenario_id is empty")
		}
		if scenario.Mode != "clean" {
			t.Errorf("scenario %s mode = %q, want clean", scenario.ScenarioID, scenario.Mode)
		}
		if scenario.Difficulty != synth.DifficultyEasy {
			t.Errorf("scenario %s difficulty = %q", scenario.ScenarioID, scenario.Difficulty)
		}
		if scenario.BreakSignature != "" {
			t.Errorf("clean scenario %s has break signature %q", scenario.ScenarioID, scenario.BreakSignature)
		}
		if len(scenario.ExpectedResolutions) == 0 {
			t.Errorf("scenario %s has no expected_resolutions", scenario.ScenarioID)
		}
		found := false
		for _, resolution := range scenario.ExpectedResolutions {
			if resolution == scenario.PrimaryResolution {
				found = true
			}
		}
		if !found {
			t.Errorf("scenario %s primary_resolution %q not in %v",
				scenario.ScenarioID, scenario.PrimaryResolution, scenario.ExpectedResolutions)
		}
		if scenario.ExpectedTier == "" {
			t.Errorf("scenario %s has no expected_tier", scenario.ScenarioID)
		}
		if len(scenario.Amounts) == 0 || len(scenario.Accounts) == 0 || len(scenario.LedgerTxnIDs) != 1 {
			t.Errorf("scenario %s is missing amounts/accounts/ledger_txn_ids: %+v", scenario.ScenarioID, scenario)
		}
		if len(scenario.ExternalRecords) != 1 {
			t.Errorf("scenario %s has %d external records, want 1", scenario.ScenarioID, len(scenario.ExternalRecords))
		}
		for _, ref := range scenario.ExternalRecords {
			links[key{ref.Source, ref.SourceRecordID}]++
		}
	}

	// Every parsed external record appears in exactly one ground-truth link.
	parsed := map[key]struct{}{}
	for _, file := range dataset.Files {
		switch {
		case strings.HasPrefix(file.Path, "psp_a/"):
			result, err := ingest.NewPSPParser().Parse(bytes.NewReader(file.Data))
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			for _, record := range result.Records {
				parsed[key{"psp_a", record.SourceRecordID}] = struct{}{}
			}
		case strings.HasPrefix(file.Path, "bank_x/"):
			result, err := ingest.NewBankParser().Parse(bytes.NewReader(file.Data))
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			for _, record := range result.Records {
				parsed[key{"bank_x", record.SourceRecordID}] = struct{}{}
			}
		}
	}
	if len(parsed) != len(links) {
		t.Errorf("parsed external records = %d, ground truth links = %d", len(parsed), len(links))
	}
	for ref := range parsed {
		if links[ref] != 1 {
			t.Errorf("external record %v has %d ground truth links, want exactly 1", ref, links[ref])
		}
	}
	for ref, count := range links {
		if _, ok := parsed[ref]; !ok {
			t.Errorf("ground truth link %v has no external record", ref)
		}
		if count > 1 {
			t.Errorf("external record %v is linked %d times", ref, count)
		}
	}
}

func TestParseableCleanFilesAndBankContinuity(t *testing.T) {
	cfg := config.DefaultSynthConfig()
	dataset := generate(t, testOptions(7, 5))

	opening := make(map[string]int64)
	for _, currency := range cfg.CurrenciesSorted() {
		opening[currency] = cfg.OpeningBalanceMinor[currency]
	}
	for _, file := range dataset.Files {
		switch {
		case strings.HasPrefix(file.Path, "psp_a/"):
			result, err := ingest.NewPSPParser().Parse(bytes.NewReader(file.Data))
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			if result.Declared.RowCount != len(result.Records) {
				t.Errorf("%s declared rows = %d, parsed = %d", file.Path, result.Declared.RowCount, len(result.Records))
			}
		case strings.HasPrefix(file.Path, "bank_x/"):
			result, err := ingest.NewBankParser().Parse(bytes.NewReader(file.Data))
			if err != nil {
				t.Fatalf("parse %s: %v", file.Path, err)
			}
			if result.Declared.RowCount != len(result.Records) {
				t.Errorf("%s declared rows = %d, parsed = %d", file.Path, result.Declared.RowCount, len(result.Records))
			}
			if len(result.Statements) != 1 {
				t.Fatalf("%s has %d statements, want 1", file.Path, len(result.Statements))
			}
			statement := result.Statements[0]
			if got, want := statement.OpeningMinor, opening[statement.Currency]; got != want {
				t.Errorf("%s opening = %d, want %d", file.Path, got, want)
			}
			var total int64
			for _, record := range result.Records {
				total += record.AmountMinor
			}
			if got, want := statement.ClosingMinor, statement.OpeningMinor+total; got != want {
				t.Errorf("%s closing = %d, want opening+total = %d", file.Path, got, want)
			}
			opening[statement.Currency] = statement.ClosingMinor
		}
	}
}

func TestSeedChangesDataset(t *testing.T) {
	first := generate(t, testOptions(1, 2))
	second := generate(t, testOptions(2, 2))
	if first.Manifest.DatasetVersion == second.Manifest.DatasetVersion {
		t.Fatal("dataset_version did not change with the seed")
	}
	firstTxns := make(map[string]string, len(first.Ledger.Transactions))
	for _, transaction := range first.Ledger.Transactions {
		firstTxns[transaction.ID] = transaction.ValueDate + "|" + transaction.Kind
	}
	changed := false
	for _, transaction := range second.Ledger.Transactions {
		if firstTxns[transaction.ID] != transaction.ValueDate+"|"+transaction.Kind {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("changing the seed did not change any ledger transaction")
	}
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
