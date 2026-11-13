package verify

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// groundTruthDateLayout is the date form used by every synth ledger, schedule,
// and external business date.
const groundTruthDateLayout = "2006-01-02"

// groundTruthScope is the (source, currency) scope projection the candidate
// pool needs.
type groundTruthScope struct {
	Source    string
	Currency  string
	Account   string
	AccountID string
}

// groundTruthWorld indexes the generated ledger document so the verifier
// scenarios can be driven from ledger.json without a database.
type groundTruthWorld struct {
	ledger    synth.LedgerDocument
	txns      map[string]synth.Transaction
	accounts  map[string]synth.Account
	scopes    map[string]groundTruthScope
	schedules map[string][]synth.FeeSchedule
}

func groundTruthScopeKey(source, currency string) string {
	return source + "\x00" + currency
}

func groundTruthScheduleKey(source, kind, currency string) string {
	return source + "\x00" + kind + "\x00" + currency
}

// buildGroundTruthWorld builds one split and indexes its ledger.json. Build is
// deterministic, so the same helper covers dev and test without a DB.
func buildGroundTruthWorld(t *testing.T, split dataset.Split) (*dataset.Dataset, *groundTruthWorld) {
	t.Helper()
	cfg, err := dataset.DefaultConfig(split)
	if err != nil {
		t.Fatalf("dataset.DefaultConfig(%s): %v", split, err)
	}
	built, err := dataset.Build(cfg)
	if err != nil {
		t.Fatalf("dataset.Build(%s): %v", split, err)
	}
	var raw []byte
	for _, file := range built.Files {
		if file.Path == "ledger.json" {
			raw = file.Data
			break
		}
	}
	if raw == nil {
		t.Fatalf("dataset %s has no ledger.json", split)
	}
	var ledger synth.LedgerDocument
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatalf("dataset %s: decode ledger.json: %v", split, err)
	}
	world := &groundTruthWorld{
		ledger:    ledger,
		txns:      make(map[string]synth.Transaction, len(ledger.Transactions)),
		accounts:  make(map[string]synth.Account, len(ledger.Accounts)),
		scopes:    make(map[string]groundTruthScope, len(ledger.ReconciliationScopes)),
		schedules: make(map[string][]synth.FeeSchedule, len(ledger.FeeSchedules)),
	}
	for _, txn := range ledger.Transactions {
		world.txns[txn.ID] = txn
	}
	for _, account := range ledger.Accounts {
		world.accounts[account.ID] = account
	}
	for _, schedule := range ledger.FeeSchedules {
		key := groundTruthScheduleKey(schedule.Source, schedule.Kind, schedule.Currency)
		world.schedules[key] = append(world.schedules[key], schedule)
	}
	for _, scope := range ledger.ReconciliationScopes {
		accountID := ""
		for _, account := range ledger.Accounts {
			if account.Code == scope.LedgerAccountCode && account.Currency == scope.Currency {
				accountID = account.ID
				break
			}
		}
		if accountID == "" {
			t.Fatalf("dataset %s: scope %s/%s references missing account %s",
				split, scope.Source, scope.Currency, scope.LedgerAccountCode)
		}
		world.scopes[groundTruthScopeKey(scope.Source, scope.Currency)] = groundTruthScope{
			Source:    scope.Source,
			Currency:  scope.Currency,
			Account:   scope.LedgerAccountCode,
			AccountID: accountID,
		}
	}
	return built, world
}

func groundTruthDate(t *testing.T, value, what string) time.Time {
	t.Helper()
	parsed, err := time.Parse(groundTruthDateLayout, value)
	if err != nil {
		t.Fatalf("%s: parse %q: %v", what, value, err)
	}
	return parsed
}

// scenarioScope resolves the psp scope a truth scenario belongs to.
func (w *groundTruthWorld) scenarioScope(t *testing.T, scenario dataset.ScenarioTruth) groundTruthScope {
	t.Helper()
	if len(scenario.ExternalRecords) != 1 {
		t.Fatalf("scenario %s: %d external records, want 1", scenario.ScenarioID, len(scenario.ExternalRecords))
	}
	if len(scenario.Amounts) == 0 {
		t.Fatalf("scenario %s has no amounts", scenario.ScenarioID)
	}
	source := scenario.ExternalRecords[0].Source
	currency := scenario.Amounts[0].Currency
	scope, ok := w.scopes[groundTruthScopeKey(source, currency)]
	if !ok {
		t.Fatalf("scenario %s: no reconciliation scope for %s/%s", scenario.ScenarioID, source, currency)
	}
	return scope
}

// derivedLedger is the verifier projection derived from one ledger
// transaction: the fee debit on fees_expense_* and the signed net on the
// scenario scope's ledger account only.
type derivedLedger struct {
	view      LedgerTxn
	feeMinor  int64
	netMinor  int64
	valueDate time.Time
}

func (w *groundTruthWorld) deriveLedger(t *testing.T, scenario dataset.ScenarioTruth, txnID string) derivedLedger {
	t.Helper()
	txn, ok := w.txns[txnID]
	if !ok {
		t.Fatalf("scenario %s: ledger transaction %s is missing", scenario.ScenarioID, txnID)
	}
	scope := w.scenarioScope(t, scenario)
	valueDate := groundTruthDate(t, txn.ValueDate, "ledger value date "+txnID)
	var netMinor, feeMinor int64
	feeSeen := 0
	scopeSeen := false
	for _, line := range txn.Lines {
		if line.AccountID == scope.AccountID {
			account, ok := w.accounts[line.AccountID]
			if !ok {
				t.Fatalf("scenario %s: ledger entry references missing account %s", scenario.ScenarioID, line.AccountID)
			}
			if account.Code != scope.Account {
				t.Fatalf("scenario %s: scope account id %s has code %s, want %s",
					scenario.ScenarioID, line.AccountID, account.Code, scope.Account)
			}
			if line.Currency != scope.Currency {
				t.Fatalf("scenario %s: scope entry currency %s, want %s", scenario.ScenarioID, line.Currency, scope.Currency)
			}
			scopeSeen = true
			switch line.Direction {
			case "debit":
				netMinor += line.AmountMinor
			case "credit":
				netMinor -= line.AmountMinor
			default:
				t.Fatalf("scenario %s: scope entry direction %q", scenario.ScenarioID, line.Direction)
			}
		}
		if isFeeAccount(line.AccountCode) {
			if line.Direction != "debit" {
				t.Fatalf("scenario %s: fee entry direction %q, want debit", scenario.ScenarioID, line.Direction)
			}
			feeSeen++
			feeMinor = line.AmountMinor
		}
	}
	if !scopeSeen {
		t.Fatalf("scenario %s: ledger transaction %s has no entry on %s", scenario.ScenarioID, txnID, scope.Account)
	}
	if feeSeen > 1 {
		t.Fatalf("scenario %s: ledger transaction %s has %d fee debits", scenario.ScenarioID, txnID, feeSeen)
	}
	return derivedLedger{
		view: LedgerTxn{
			ID:        txn.ID,
			ValueDate: valueDate,
			Currency:  scope.Currency,
			FeeMinor:  feeMinor,
			NetMinor:  netMinor,
		},
		feeMinor:  feeMinor,
		netMinor:  netMinor,
		valueDate: valueDate,
	}
}

// externalRecord builds the external projection from truth. It needs no CSV
// parse: truth carries the record identity, amount, and currency, and the
// ledger transaction carries the value date used by the schedule and reference
// windows.
func externalRecord(t *testing.T, scenario dataset.ScenarioTruth, amountMinor int64, at time.Time) ExternalRecord {
	t.Helper()
	if len(scenario.ExternalRecords) != 1 {
		t.Fatalf("scenario %s: %d external records, want 1", scenario.ScenarioID, len(scenario.ExternalRecords))
	}
	return ExternalRecord{
		Source:         scenario.ExternalRecords[0].Source,
		SourceRecordID: scenario.ExternalRecords[0].SourceRecordID,
		Kind:           "charge",
		Currency:       scenario.Amounts[0].Currency,
		AmountMinor:    amountMinor,
		OccurredAt:     at,
		ValueDate:      at,
	}
}

func adjustmentLines(lines []dataset.ExpectedLine) []AdjustmentLine {
	out := make([]AdjustmentLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, AdjustmentLine{
			AccountCode: line.AccountCode,
			Direction:   line.Direction,
			AmountMinor: line.AmountMinor,
			Currency:    line.Currency,
		})
	}
	return out
}

// feeSchedules converts every generated schedule for the external's
// source/kind/currency. The pure evaluator performs the inclusive date
// selection and fails closed on zero or multiple matches.
func (w *groundTruthWorld) feeSchedules(t *testing.T, external ExternalRecord) []FeeSchedule {
	t.Helper()
	key := groundTruthScheduleKey(external.Source, external.Kind, external.Currency)
	raw := w.schedules[key]
	out := make([]FeeSchedule, 0, len(raw))
	for _, schedule := range raw {
		converted := FeeSchedule{
			ID:         schedule.ID,
			Source:     schedule.Source,
			Kind:       schedule.Kind,
			Currency:   schedule.Currency,
			PctBPS:     schedule.PctBps,
			FixedMinor: schedule.FixedMinor,
			ValidFrom:  groundTruthDate(t, schedule.ValidFrom, "schedule valid_from"),
		}
		if schedule.ValidTo != nil {
			converted.ValidTo = groundTruthDate(t, *schedule.ValidTo, "schedule valid_to")
		}
		out = append(out, converted)
	}
	return out
}

func runGroundTruthFeeVariance(t *testing.T, w *groundTruthWorld, scenario dataset.ScenarioTruth) Result {
	t.Helper()
	if scenario.ExpectedAdjustment == nil {
		t.Fatalf("scenario %s: fee_variance has no expected adjustment", scenario.ScenarioID)
	}
	if len(scenario.LedgerTxnIDs) != 1 {
		t.Fatalf("scenario %s: %d ledger txns, want 1", scenario.ScenarioID, len(scenario.LedgerTxnIDs))
	}
	if len(scenario.Amounts) != 2 {
		t.Fatalf("scenario %s: %d amounts, want 2", scenario.ScenarioID, len(scenario.Amounts))
	}
	derived := w.deriveLedger(t, scenario, scenario.LedgerTxnIDs[0])
	if derived.netMinor != scenario.Amounts[0].AmountMinor {
		t.Fatalf("scenario %s: ledger net %d, truth %d", scenario.ScenarioID, derived.netMinor, scenario.Amounts[0].AmountMinor)
	}
	externalNet := scenario.Amounts[1].AmountMinor
	delta := scenario.Amounts[0].AmountMinor - externalNet
	if delta <= 0 {
		t.Fatalf("scenario %s: fee delta %d is not positive", scenario.ScenarioID, delta)
	}
	external := externalRecord(t, scenario, externalNet, derived.valueDate)
	external.FeeMinor = derived.feeMinor + delta
	return VerifyFeeVariance(FeeVarianceInput{
		Claimed:   PatternFeeVariance,
		External:  external,
		Ledger:    derived.view,
		Schedules: w.feeSchedules(t, external),
		Lines:     adjustmentLines(scenario.ExpectedAdjustment.Lines),
	}, DefaultOptions())
}

// signedNetOn returns the signed net of one transaction on the scope account,
// restricted to the scope currency exactly as the service's candidate query is.
func (w *groundTruthWorld) signedNetOn(t *testing.T, txn synth.Transaction, scope groundTruthScope) (int64, bool) {
	t.Helper()
	var net int64
	seen := false
	for _, line := range txn.Lines {
		if line.AccountID != scope.AccountID || line.Currency != scope.Currency {
			continue
		}
		seen = true
		switch line.Direction {
		case "debit":
			net += line.AmountMinor
		case "credit":
			net -= line.AmountMinor
		default:
			t.Fatalf("txn %s: scope entry direction %q", txn.ID, line.Direction)
		}
	}
	return net, seen
}

func runGroundTruthReferenceTypo(t *testing.T, w *groundTruthWorld, scenario dataset.ScenarioTruth) Result {
	t.Helper()
	if scenario.ExpectedLink == nil || len(scenario.ExpectedLink.LedgerTxnIDs) != 1 {
		t.Fatalf("scenario %s: reference_typo has no single expected link", scenario.ScenarioID)
	}
	if len(scenario.Amounts) != 1 {
		t.Fatalf("scenario %s: %d amounts, want 1", scenario.ScenarioID, len(scenario.Amounts))
	}
	expectedID := scenario.ExpectedLink.LedgerTxnIDs[0]
	derived := w.deriveLedger(t, scenario, expectedID)
	if derived.netMinor != scenario.Amounts[0].AmountMinor {
		t.Fatalf("scenario %s: ledger net %d, truth %d", scenario.ScenarioID, derived.netMinor, scenario.Amounts[0].AmountMinor)
	}
	external := externalRecord(t, scenario, scenario.Amounts[0].AmountMinor, derived.valueDate)
	scope := w.scenarioScope(t, scenario)
	if scope.Source != external.Source {
		t.Fatalf("scenario %s: scope source %s, external source %s", scenario.ScenarioID, scope.Source, external.Source)
	}

	candidates := make([]Candidate, 0, 4)
	for _, txn := range w.ledger.Transactions {
		net, ok := w.signedNetOn(t, txn, scope)
		if !ok || net != external.AmountMinor {
			continue
		}
		valueDate := groundTruthDate(t, txn.ValueDate, "candidate value date")
		if !withinDateWindow(referenceDate(external), valueDate, DefaultReferenceWindowDays) {
			continue
		}
		candidates = append(candidates, Candidate{
			LedgerTxnID: txn.ID,
			AmountMinor: net,
			Currency:    scope.Currency,
			ValueDate:   valueDate,
		})
	}
	return VerifyReferenceTypo(ReferenceTypoInput{
		Claimed:           PatternReferenceTypo,
		External:          external,
		Candidates:        candidates,
		ProposedLedgerIDs: scenario.ExpectedLink.LedgerTxnIDs,
	}, DefaultOptions())
}

func runGroundTruthFXRounding(t *testing.T, w *groundTruthWorld, scenario dataset.ScenarioTruth) Result {
	t.Helper()
	if scenario.ExpectedAdjustment == nil {
		t.Fatalf("scenario %s: fx_rounding has no expected adjustment", scenario.ScenarioID)
	}
	if len(scenario.LedgerTxnIDs) != 1 {
		t.Fatalf("scenario %s: %d ledger txns, want 1", scenario.ScenarioID, len(scenario.LedgerTxnIDs))
	}
	if len(scenario.Amounts) < 2 {
		t.Fatalf("scenario %s: %d amounts, want target and source", scenario.ScenarioID, len(scenario.Amounts))
	}
	derived := w.deriveLedger(t, scenario, scenario.LedgerTxnIDs[0])
	if derived.netMinor != scenario.Amounts[0].AmountMinor {
		t.Fatalf("scenario %s: ledger net %d, truth %d", scenario.ScenarioID, derived.netMinor, scenario.Amounts[0].AmountMinor)
	}
	external := externalRecord(t, scenario, scenario.Amounts[1].AmountMinor, derived.valueDate)
	return VerifyFXRounding(FXRoundingInput{
		Claimed:  PatternFXRounding,
		External: external,
		Ledger:   derived.view,
		// The committed dev/test datasets seed no fx_rates. The verifier must
		// fail closed with missing_rate rather than invent or default a rate.
		Rates: nil,
		Lines: adjustmentLines(scenario.ExpectedAdjustment.Lines),
	}, DefaultOptions())
}

// TestGroundTruthDatasetPatterns drives the pure verifiers from the committed
// dev and test ground truth. It is a normal unit test: no DB, no network, and a
// deterministic Build. For every truth scenario it builds the verifier input
// from ledger.json and truth alone, asserts fee_variance and reference_typo
// pass, and asserts fx_rounding fails closed with missing_rate because the
// datasets carry no fx_rates rows.
func TestGroundTruthDatasetPatterns(t *testing.T) {
	type patternCounts struct {
		scenarios   int
		passes      int
		failures    int
		falsePasses int
	}
	aggregate := map[string]*patternCounts{}
	countFor := func(pattern string) *patternCounts {
		counts := aggregate[pattern]
		if counts == nil {
			counts = &patternCounts{}
			aggregate[pattern] = counts
		}
		return counts
	}

	for _, split := range []dataset.Split{dataset.Dev, dataset.Test} {
		t.Run(string(split), func(t *testing.T) {
			built, world := buildGroundTruthWorld(t, split)
			for _, scenario := range built.Truth.Scenarios {
				var (
					result       Result
					expectedPass bool
				)
				switch scenario.Mode {
				case modes.FeeVariance:
					result = runGroundTruthFeeVariance(t, world, scenario)
					expectedPass = true
				case modes.ReferenceTypo:
					result = runGroundTruthReferenceTypo(t, world, scenario)
					expectedPass = true
				case modes.FXRounding:
					result = runGroundTruthFXRounding(t, world, scenario)
					expectedPass = false
				default:
					continue
				}

				counts := countFor(scenario.Mode)
				counts.scenarios++
				if result.Passed {
					counts.passes++
				} else {
					counts.failures++
				}
				if result.Passed != expectedPass {
					if result.Passed {
						counts.falsePasses++
						t.Errorf("%s scenario %s: false pass (truth does not support a pass)", scenario.Mode, scenario.ScenarioID)
					} else {
						t.Errorf("%s scenario %s: expected pass, got failure %s", scenario.Mode, scenario.ScenarioID, result.Details.Reason)
					}
				}
				if scenario.Mode == modes.FXRounding && !result.Passed && result.Details.Reason != ReasonMissingRate {
					t.Errorf("%s scenario %s: failure reason %q, want %q",
						scenario.Mode, scenario.ScenarioID, result.Details.Reason, ReasonMissingRate)
				}
			}
		})
	}

	for _, pattern := range []string{modes.FeeVariance, modes.ReferenceTypo} {
		counts := aggregate[pattern]
		if counts == nil || counts.passes == 0 {
			t.Errorf("%s: no passing ground-truth scenario", pattern)
		}
	}
	for pattern, counts := range aggregate {
		if counts.falsePasses != 0 {
			t.Errorf("%s: %d false pass(es)", pattern, counts.falsePasses)
		}
		t.Logf("%s: scenarios=%d passes=%d failures=%d false_passes=%d",
			pattern, counts.scenarios, counts.passes, counts.failures, counts.falsePasses)
	}
	if len(aggregate) == 0 {
		t.Fatal("ground truth contained no verifier pattern scenarios")
	}
}
