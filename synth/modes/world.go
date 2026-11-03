package modes

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1.
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/synth"
)

const (
	dateLayout      = "2006-01-02"
	timestampLayout = time.RFC3339
)

// pspColumns is the exact psp_a header from docs/formats.md. The modes package
// re-renders only psp_a files; bank_x files are copied through untouched.
var pspColumns = []string{
	"record_id",
	"type",
	"occurred_at",
	"settled_at",
	"gross",
	"fee",
	"net",
	"currency",
	"reference",
	"batch_id",
	"memo",
}

// modeNamespace is a fixed UUIDv5 namespace for logical IDs created by the
// modes package. It is deliberately distinct from synth's namespace so a
// reviewer can tell a mode-created ledger txn from an M1-09 txn.
var modeNamespace = [16]byte{
	0x3b, 0x91, 0x74, 0xc2, 0x0e, 0x4d, 0x4a, 0x8f,
	0x96, 0x21, 0xb7, 0x5e, 0x18, 0x6a, 0xf0, 0x42,
}

// pspRow is one parsed psp_a data row. Amounts are signed integer minor units;
// GrossMinor - FeeMinor always equals NetMinor.
type pspRow struct {
	RecordID   string
	Type       string
	OccurredAt time.Time
	SettledAt  *time.Time
	GrossMinor int64
	FeeMinor   int64
	NetMinor   int64
	Currency   string
	Reference  string
	BatchID    string
	Memo       string
}

// pspFile is one parsed psp_a batch. The declared total is recomputed on render
// so a mutation can never leave the trailer stale.
type pspFile struct {
	Path        string
	BatchID     string
	Currency    string
	PeriodStart string
	PeriodEnd   string
	Rows        []pspRow
}

// World is the mutable dataset a Mode operates on.
type World struct {
	Seed         int64
	Difficulty   string
	Accounts     []synth.Account
	Customers    []synth.Customer
	Merchants    []synth.Merchant
	Schedules    []synth.FeeSchedule
	Scopes       []synth.Scope
	Transactions []synth.Transaction
	Truth        []synth.Scenario

	clean         []synth.Scenario
	psp           map[string]*pspFile
	bankFiles     []synth.File
	manifest      synth.Manifest
	sources       map[string]config.SourceConfig
	usedTxns      map[string]struct{}
	txnByID       map[string]int
	externalCount int
	variant       int
}

// newWorld copies the mutable parts of a clean dataset and parses its psp_a
// files. Parsing is pure and never calls the database or the wall clock.
func newWorld(dataset *synth.Dataset, sources map[string]config.SourceConfig) (*World, error) {
	if len(dataset.Files) == 0 {
		return nil, fmt.Errorf("%w: dataset has no files", ErrInvalidWorld)
	}
	world := &World{
		Seed:          dataset.Manifest.Seed,
		Difficulty:    dataset.Manifest.Difficulty,
		Accounts:      append([]synth.Account(nil), dataset.Ledger.Accounts...),
		Customers:     append([]synth.Customer(nil), dataset.Ledger.Customers...),
		Merchants:     append([]synth.Merchant(nil), dataset.Ledger.Merchants...),
		Schedules:     append([]synth.FeeSchedule(nil), dataset.Ledger.FeeSchedules...),
		Scopes:        append([]synth.Scope(nil), dataset.Ledger.ReconciliationScopes...),
		Transactions:  append([]synth.Transaction(nil), dataset.Ledger.Transactions...),
		Truth:         append([]synth.Scenario(nil), dataset.Truth.Scenarios...),
		clean:         append([]synth.Scenario(nil), dataset.Truth.Scenarios...),
		psp:           make(map[string]*pspFile),
		manifest:      dataset.Manifest,
		sources:       sources,
		usedTxns:      make(map[string]struct{}),
		txnByID:       make(map[string]int, len(dataset.Ledger.Transactions)),
		externalCount: dataset.Manifest.Counts.ExternalRecords,
	}
	for i, txn := range world.Transactions {
		world.txnByID[txn.ID] = i
	}
	for _, file := range dataset.Files {
		switch {
		case strings.HasPrefix(file.Path, synth.SourcePSP+"/"):
			parsed, err := parsePSPFile(file)
			if err != nil {
				return nil, err
			}
			world.psp[file.Path] = parsed
		case strings.HasPrefix(file.Path, synth.SourceBank+"/"):
			world.bankFiles = append(world.bankFiles, synth.File{
				Path: file.Path,
				Data: append([]byte(nil), file.Data...),
			})
		}
	}
	if len(world.psp) == 0 {
		return nil, fmt.Errorf("%w: dataset has no psp_a files", ErrInvalidWorld)
	}
	return world, nil
}

func parsePSPFile(file synth.File) (*pspFile, error) {
	newline := bytes.IndexByte(file.Data, '\n')
	if newline < 0 {
		return nil, fmt.Errorf("%w: %s has no metadata line", ErrInvalidWorld, file.Path)
	}
	metadataLine := strings.TrimSpace(string(file.Data[:newline]))
	if !strings.HasPrefix(metadataLine, "# ") {
		return nil, fmt.Errorf("%w: %s metadata line is malformed", ErrInvalidWorld, file.Path)
	}
	metadata := make(map[string]string)
	for _, token := range strings.Fields(strings.TrimPrefix(metadataLine, "# ")) {
		key, value, ok := strings.Cut(token, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("%w: %s metadata token %q is malformed",
				ErrInvalidWorld, file.Path, token)
		}
		metadata[key] = value
	}
	currency, err := money.ParseCurrency(metadata["currency"])
	if err != nil {
		return nil, fmt.Errorf("%w: %s currency %q: %v",
			ErrInvalidWorld, file.Path, metadata["currency"], err)
	}
	parsed := &pspFile{
		Path:        file.Path,
		BatchID:     metadata["batch"],
		Currency:    metadata["currency"],
		PeriodStart: metadata["period_start"],
		PeriodEnd:   metadata["period_end"],
	}

	reader := csv.NewReader(bytes.NewReader(file.Data[newline+1:]))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidWorld, file.Path, err)
	}
	if len(records) == 0 || strings.Join(records[0], ",") != strings.Join(pspColumns, ",") {
		return nil, fmt.Errorf("%w: %s header does not match psp_a", ErrInvalidWorld, file.Path)
	}
	for index, fields := range records[1:] {
		if len(fields) != len(pspColumns) {
			return nil, fmt.Errorf("%w: %s row %d has %d fields",
				ErrInvalidWorld, file.Path, index+2, len(fields))
		}
		row, err := parsePSPRow(fields, currency)
		if err != nil {
			return nil, fmt.Errorf("%w: %s row %d: %v", ErrInvalidWorld, file.Path, index+2, err)
		}
		parsed.Rows = append(parsed.Rows, row)
	}
	return parsed, nil
}

func parsePSPRow(fields []string, currency money.Currency) (pspRow, error) {
	occurred, err := time.Parse(timestampLayout, fields[2])
	if err != nil {
		return pspRow{}, fmt.Errorf("occurred_at %q: %v", fields[2], err)
	}
	var settled *time.Time
	if fields[3] != "" {
		value, err := time.Parse(timestampLayout, fields[3])
		if err != nil {
			return pspRow{}, fmt.Errorf("settled_at %q: %v", fields[3], err)
		}
		settled = &value
	}
	gross, err := money.ParseDecimal(fields[4], currency)
	if err != nil {
		return pspRow{}, fmt.Errorf("gross %q: %v", fields[4], err)
	}
	fee, err := money.ParseDecimal(fields[5], currency)
	if err != nil {
		return pspRow{}, fmt.Errorf("fee %q: %v", fields[5], err)
	}
	net, err := money.ParseDecimal(fields[6], currency)
	if err != nil {
		return pspRow{}, fmt.Errorf("net %q: %v", fields[6], err)
	}
	return pspRow{
		RecordID:   fields[0],
		Type:       fields[1],
		OccurredAt: occurred.UTC(),
		SettledAt:  settled,
		GrossMinor: gross.AmountMinor(),
		FeeMinor:   fee.AmountMinor(),
		NetMinor:   net.AmountMinor(),
		Currency:   fields[7],
		Reference:  fields[8],
		BatchID:    fields[9],
		Memo:       fields[10],
	}, nil
}

// render writes the batch in the canonical psp_a format. Rows are sorted by
// record_id and rows/total are recomputed, so map iteration and mutation order
// cannot leak into the bytes.
func (f *pspFile) render() ([]byte, error) {
	rows := append([]pspRow(nil), f.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].RecordID < rows[j].RecordID })
	currency, err := money.ParseCurrency(f.Currency)
	if err != nil {
		return nil, fmt.Errorf("%w: psp file %s currency: %v", ErrInvalidWorld, f.Path, err)
	}
	var total int64
	for _, row := range rows {
		total += row.NetMinor
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# rows=%d total=%s currency=%s batch=%s",
		len(rows), money.FromMinor(total, currency).DecimalString(), f.Currency, f.BatchID)
	if f.PeriodStart != "" && f.PeriodEnd != "" {
		fmt.Fprintf(&buf, " period_start=%s period_end=%s", f.PeriodStart, f.PeriodEnd)
	}
	buf.WriteByte('\n')
	buf.WriteString(strings.Join(pspColumns, ","))
	buf.WriteByte('\n')
	writer := csv.NewWriter(&buf)
	for _, row := range rows {
		settled := ""
		if row.SettledAt != nil {
			settled = row.SettledAt.UTC().Format(timestampLayout)
		}
		if err := writer.Write([]string{
			row.RecordID,
			row.Type,
			row.OccurredAt.UTC().Format(timestampLayout),
			settled,
			money.FromMinor(row.GrossMinor, currency).DecimalString(),
			money.FromMinor(row.FeeMinor, currency).DecimalString(),
			money.FromMinor(row.NetMinor, currency).DecimalString(),
			row.Currency,
			row.Reference,
			row.BatchID,
			row.Memo,
		}); err != nil {
			return nil, fmt.Errorf("synth/modes: render %s: %w", f.Path, err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("synth/modes: render %s: %w", f.Path, err)
	}
	return buf.Bytes(), nil
}

func (f *pspFile) date() time.Time {
	if parsed, err := time.ParseInLocation(dateLayout, f.PeriodStart, time.UTC); err == nil {
		return parsed
	}
	if len(f.Rows) > 0 {
		occurred := f.Rows[0].OccurredAt.UTC()
		return time.Date(occurred.Year(), occurred.Month(), occurred.Day(), 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func (f *pspFile) at(hour, minute int) time.Time {
	date := f.date()
	return time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, time.UTC)
}

func (f *pspFile) compactDate() string {
	return f.date().Format("20060102")
}

// pspPaths returns the parsed batch paths in ascending order. This is the only
// iteration order used for selection and rendering.
func (w *World) pspPaths() []string {
	paths := make([]string, 0, len(w.psp))
	for path := range w.psp {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (w *World) firstPSPPathForCurrency(currency string) string {
	for _, path := range w.pspPaths() {
		if w.psp[path].Currency == currency {
			return path
		}
	}
	return ""
}

func (w *World) accountID(code string) string {
	for _, account := range w.Accounts {
		if account.Code == code {
			return account.ID
		}
	}
	return ""
}

func (w *World) txn(id string) (synth.Transaction, bool) {
	index, ok := w.txnByID[id]
	if !ok {
		return synth.Transaction{}, false
	}
	return w.Transactions[index], true
}

func (w *World) nextVariant() int {
	w.variant++
	return w.variant
}

// target is one unused clean ledger transaction together with its psp_a row.
type target struct {
	Scenario synth.Scenario
	Txn      synth.Transaction
	File     *pspFile
	RowIndex int
}

// nextTarget claims the first unused clean psp_a scenario of kind in the
// deterministic clean order, removes its clean ground-truth row, and returns
// the pair. Claiming prevents two modes from mutating the same transaction.
func (w *World) nextTarget(kind string) (*target, error) {
	for _, scenario := range w.clean {
		if len(scenario.LedgerTxnIDs) != 1 || len(scenario.ExternalRecords) != 1 {
			continue
		}
		if scenario.ExternalRecords[0].Source != synth.SourcePSP {
			continue
		}
		if _, used := w.usedTxns[scenario.LedgerTxnIDs[0]]; used {
			continue
		}
		txn, ok := w.txn(scenario.LedgerTxnIDs[0])
		if !ok || txn.Kind != kind {
			continue
		}
		file, index := w.findPSPRow(scenario.ExternalRecords[0].SourceRecordID)
		if file == nil {
			continue
		}
		w.usedTxns[txn.ID] = struct{}{}
		w.removeScenario(scenario.ScenarioID)
		return &target{Scenario: scenario, Txn: txn, File: file, RowIndex: index}, nil
	}
	return nil, fmt.Errorf("%w: no unused psp_a %s scenario", ErrInsufficientData, kind)
}

func (w *World) findPSPRow(recordID string) (*pspFile, int) {
	for _, path := range w.pspPaths() {
		file := w.psp[path]
		for index, row := range file.Rows {
			if row.RecordID == recordID {
				return file, index
			}
		}
	}
	return nil, -1
}

func (w *World) removeScenario(scenarioID string) {
	filtered := w.Truth[:0]
	for _, scenario := range w.Truth {
		if scenario.ScenarioID != scenarioID {
			filtered = append(filtered, scenario)
		}
	}
	w.Truth = filtered
}

func (w *World) addScenario(scenario synth.Scenario) {
	w.Truth = append(w.Truth, scenario)
}

func (w *World) addPSPRow(path string, row pspRow) {
	file, ok := w.psp[path]
	if !ok {
		return
	}
	file.Rows = append(file.Rows, row)
	w.externalCount++
}

func (w *World) removePSPRow(path, recordID string) error {
	file, ok := w.psp[path]
	if !ok {
		return fmt.Errorf("%w: psp file %q not found", ErrInvalidWorld, path)
	}
	for index, row := range file.Rows {
		if row.RecordID != recordID {
			continue
		}
		file.Rows = append(file.Rows[:index], file.Rows[index+1:]...)
		w.externalCount--
		return nil
	}
	return fmt.Errorf("%w: psp_a row %q not found", ErrInvalidWorld, recordID)
}

// failureScenario builds the HD-3 scenario for one mutated transaction. The
// break signature is match.BreakCandidate.Signature(), the same canonical form
// M1-14 stores in breaks.signature.
func (w *World) failureScenario(mode string, txn synth.Transaction, amounts []synth.Amount,
	refs []synth.RecordRef, resolutions []string, primary, tier, side string, delta int64) synth.Scenario {
	currency := txnCurrency(txn)
	ledgerIDs := []string{txn.ID}
	external := refs
	matchRefs := make([]match.RecordRef, 0, len(external))
	for _, ref := range external {
		matchRefs = append(matchRefs, match.RecordRef{
			Source:         ref.Source,
			SourceRecordID: ref.SourceRecordID,
		})
	}
	signature := match.BreakCandidate{
		ScopeID:         w.scopeID(synth.SourcePSP, currency),
		Side:            match.BreakSide(side),
		LedgerTxnIDs:    ledgerIDs,
		ExternalRecords: matchRefs,
		Currency:        currency,
		DeltaMinor:      delta,
	}.Signature()
	return synth.Scenario{
		ScenarioID:          logicalID(w.Seed, "scenario", mode, txn.ID),
		Mode:                mode,
		Difficulty:          w.Difficulty,
		BreakSignature:      signature,
		ExpectedResolutions: append([]string(nil), resolutions...),
		PrimaryResolution:   primary,
		ExpectedTier:        tier,
		Amounts:             amounts,
		Accounts:            accountsFor(txn),
		LedgerTxnIDs:        ledgerIDs,
		ExternalRecords:     external,
	}
}

func (w *World) scopeID(source, currency string) string {
	return logicalID(w.Seed, "scope", source, currency)
}

// CurrencyCodes returns the dataset currencies in ascending order.
func (w *World) CurrencyCodes() []string {
	seen := make(map[string]struct{}, len(w.Scopes))
	var codes []string
	for _, scope := range w.Scopes {
		if _, ok := seen[scope.Currency]; ok {
			continue
		}
		seen[scope.Currency] = struct{}{}
		codes = append(codes, scope.Currency)
	}
	sort.Strings(codes)
	return codes
}

// addTransaction appends a mode-created ledger transaction.
func (w *World) addTransaction(txn synth.Transaction) {
	w.txnByID[txn.ID] = len(w.Transactions)
	w.Transactions = append(w.Transactions, txn)
}

func txnCurrency(txn synth.Transaction) string {
	if len(txn.Lines) == 0 {
		return ""
	}
	return txn.Lines[0].Currency
}

func accountsFor(txn synth.Transaction) []string {
	seen := make(map[string]struct{}, len(txn.Lines))
	accounts := make([]string, 0, len(txn.Lines))
	for _, line := range txn.Lines {
		if _, ok := seen[line.AccountCode]; ok {
			continue
		}
		seen[line.AccountCode] = struct{}{}
		accounts = append(accounts, line.AccountCode)
	}
	sort.Strings(accounts)
	return accounts
}

func amount(currency string, minor int64) synth.Amount {
	return synth.Amount{Currency: currency, AmountMinor: minor}
}

// render assembles the failure-mode dataset. It recomputes the config hash and
// dataset version from the clean hash plus the canonical mix and noise knobs so
// two mixes can never share a dataset_version.
func (w *World) render(cfg Config) (*Result, error) {
	noise, err := NoiseFor(w.Difficulty)
	if err != nil {
		return nil, err
	}
	modeCounts := map[string]int{CleanMode: 0}
	for _, name := range PartAModes {
		modeCounts[name] = 0
	}
	difficultyCounts := make(map[string]int)
	for _, scenario := range w.Truth {
		modeCounts[scenario.Mode]++
		difficultyCounts[scenario.Difficulty]++
	}
	configHash := hashHex(w.manifest.ConfigHash + "|" + cfg.canonical() + "|noise=" + noise.canonical())
	version := datasetVersion(w.Seed, configHash)

	counts := w.manifest.Counts
	counts.LedgerTxns = len(w.Transactions)
	counts.ExternalRecords = w.externalCount
	counts.GroundTruthLinks = 0
	for _, scenario := range w.Truth {
		counts.GroundTruthLinks += len(scenario.ExternalRecords)
	}
	base := w.manifest
	base.ConfigHash = configHash
	base.DatasetVersion = version
	base.Counts = counts
	manifest := Manifest{
		Manifest:         base,
		ModeCounts:       modeCounts,
		DifficultyCounts: difficultyCounts,
		Noise:            noise,
	}
	ledger := synth.LedgerDocument{
		GeneratorVersion:     synth.GeneratorVersion,
		DatasetVersion:       version,
		Accounts:             w.Accounts,
		Customers:            w.Customers,
		Merchants:            w.Merchants,
		FeeSchedules:         w.Schedules,
		ReconciliationScopes: w.Scopes,
		Transactions:         w.Transactions,
	}
	scenarios := append([]synth.Scenario(nil), w.Truth...)
	sort.Slice(scenarios, func(i, j int) bool { return scenarios[i].ScenarioID < scenarios[j].ScenarioID })
	truth := synth.GroundTruth{DatasetVersion: version, Scenarios: scenarios}

	files, err := w.renderFiles(manifest, ledger, truth)
	if err != nil {
		return nil, err
	}
	return &Result{Manifest: manifest, Ledger: ledger, Truth: truth, Files: files}, nil
}

func (w *World) renderFiles(manifest Manifest, ledger synth.LedgerDocument,
	truth synth.GroundTruth) ([]synth.File, error) {
	documents := []struct {
		name    string
		payload any
	}{
		{name: "manifest.json", payload: manifest},
		{name: "ledger.json", payload: ledger},
		{name: "ground_truth.json", payload: truth},
	}
	files := make([]synth.File, 0, len(documents)+len(w.psp)+len(w.bankFiles))
	for _, document := range documents {
		data, err := jsonDocument(document.payload)
		if err != nil {
			return nil, fmt.Errorf("synth/modes: render %s: %w", document.name, err)
		}
		files = append(files, synth.File{Path: document.name, Data: data})
	}
	for _, path := range w.pspPaths() {
		data, err := w.psp[path].render()
		if err != nil {
			return nil, err
		}
		files = append(files, synth.File{Path: path, Data: data})
	}
	files = append(files, w.bankFiles...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i := 1; i < len(files); i++ {
		if files[i].Path == files[i-1].Path {
			return nil, fmt.Errorf("synth/modes: duplicate output path %q", files[i].Path)
		}
	}
	return files, nil
}

func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func hashHex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func datasetVersion(seed int64, configHash string) string {
	return hashHex(fmt.Sprintf("%s|%d|%s", synth.GeneratorVersion, seed, configHash))
}

// logicalID is a UUIDv5 over (modeNamespace, seed, parts...). The same seed
// and parts always produce the same UUID on every machine and process.
func logicalID(seed int64, parts ...string) string {
	hash := sha1.New() //nolint:gosec // UUIDv5 is defined over SHA-1.
	_, _ = hash.Write(modeNamespace[:])
	_, _ = hash.Write([]byte("|seed="))
	_, _ = hash.Write([]byte(strconv.FormatInt(seed, 10)))
	for _, part := range parts {
		_, _ = hash.Write([]byte("|"))
		_, _ = hash.Write([]byte(part))
	}
	sum := hash.Sum(nil)
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(id[:])
	return strings.Join([]string{
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32],
	}, "-")
}
