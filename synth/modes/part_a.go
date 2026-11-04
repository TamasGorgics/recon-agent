package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/synth"
)

// FX conversion used by fx_rounding. It is a fixed integer rate so the residual
// is exact and never depends on floating point (FIN-01/FIN-06).
const (
	// FXRateNum is the numerator of the source-to-target minor-unit rate.
	FXRateNum int64 = 10_875
	// FXRateDen is the denominator of the source-to-target minor-unit rate.
	FXRateDen int64 = 10_000
)

// ConversionMinor converts sourceMinor at FXRateNum/FXRateDen. It returns the
// truncated amount (the ledger's view) and the half-up rounded amount (the
// PSP's view). Their difference is the exact conversion residual in minor
// units and is always zero or one.
func ConversionMinor(sourceMinor int64) (truncated, rounded int64) {
	exact := sourceMinor * FXRateNum
	truncated = exact / FXRateDen
	rounded = truncated
	if (exact%FXRateDen)*2 >= FXRateDen {
		rounded++
	}
	return truncated, rounded
}

// applyFeeVariance recharges the psp_a fee by a small delta. The ledger fee is
// the schedule fee; the external fee is schedule fee + delta, and net is
// recomputed so the file stays gross - fee = net.
func applyFeeVariance(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	row := &target.File.Rows[target.RowIndex]
	ledgerNet := row.NetMinor
	delta := int64(1 + w.nextVariant()%3)
	row.FeeMinor += delta
	row.NetMinor = row.GrossMinor - row.FeeMinor
	scenario := w.failureScenario(FeeVariance, target.Txn,
		[]synth.Amount{amount(row.Currency, ledgerNet), amount(row.Currency, row.NetMinor)},
		target.Scenario.ExternalRecords,
		[]string{"adjustment"}, "adjustment", "T0",
		string(match.SideAmountMismatch), row.NetMinor-ledgerNet)
	w.addScenario(scenario)
	return nil
}

// applyFXRounding adds one converted charge. The ledger books the truncated
// conversion and the PSP settles the half-up conversion, so the external minus
// ledger delta is exactly ConversionMinor's residual.
func applyFXRounding(w *World) error {
	targetCurrency, sourceCurrency, err := w.fxPair()
	if err != nil {
		return err
	}
	path := w.firstPSPPathForCurrency(targetCurrency)
	if path == "" {
		return fmt.Errorf("%w: no psp_a file for %s", ErrInvalidWorld, targetCurrency)
	}
	file := w.psp[path]
	variant := w.nextVariant()
	sourceMinor := int64(123_045 + variant*7_919)
	truncated, rounded := ConversionMinor(sourceMinor)
	for rounded == truncated {
		sourceMinor++
		truncated, rounded = ConversionMinor(sourceMinor)
	}
	at := file.at(15, 0)
	recordID := fmt.Sprintf("psp_fx_%s_%s_%03d", file.compactDate(),
		strings.ToLower(targetCurrency), variant)
	reference := "REF-FX-" + recordID
	txn, err := w.newConvertedCharge(targetCurrency, truncated, reference, recordID, at)
	if err != nil {
		return err
	}
	w.addPSPRow(path, pspRow{
		RecordID:   recordID,
		Type:       "charge",
		OccurredAt: at,
		SettledAt:  &at,
		GrossMinor: rounded,
		NetMinor:   rounded,
		Currency:   targetCurrency,
		Reference:  reference,
		BatchID:    file.BatchID,
		Memo:       "synthetic FX conversion from " + sourceCurrency,
	})
	scenario := w.failureScenario(FXRounding, txn,
		[]synth.Amount{
			amount(targetCurrency, truncated),
			amount(targetCurrency, rounded),
			amount(sourceCurrency, sourceMinor),
		},
		[]synth.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}},
		[]string{"adjustment"}, "adjustment", "T0",
		string(match.SideAmountMismatch), rounded-truncated)
	w.addScenario(scenario)
	return nil
}

// applyMissingSettlement removes the psp_a charge row and leaves the ledger
// charge alone: there is no external record to link.
func applyMissingSettlement(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	row := target.File.Rows[target.RowIndex]
	currency, ledgerNet := row.Currency, row.NetMinor
	if err := w.removePSPRow(target.File.Path, row.RecordID); err != nil {
		return err
	}
	scenario := w.failureScenario(MissingSettlement, target.Txn,
		[]synth.Amount{amount(currency, ledgerNet)}, nil,
		[]string{"request_info", "escalate"}, "request_info", "T1",
		string(match.SideLedgerOnly), ledgerNet)
	w.addScenario(scenario)
	return nil
}

// applyLateSettlement moves a charge's occurrence and settlement time after the
// psp_a cutoff. The break signature is the ledger-only break as of the cutoff;
// the external record arrives after it, so expected_resolutions is ["link"].
func applyLateSettlement(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	row := &target.File.Rows[target.RowIndex]
	cutoff, err := sourceCutoff(w.sources, synth.SourcePSP, row.OccurredAt)
	if err != nil {
		return err
	}
	late := cutoff.Add(90 * time.Minute)
	row.OccurredAt = late
	settled := late
	row.SettledAt = &settled
	currency, ledgerNet := row.Currency, row.NetMinor
	scenario := w.failureScenario(LateSettlement, target.Txn,
		[]synth.Amount{amount(currency, ledgerNet)}, target.Scenario.ExternalRecords,
		[]string{"link"}, "link", "T1",
		string(match.SideLedgerOnly), ledgerNet)
	w.addScenario(scenario)
	return nil
}

// applyDuplicateCharge adds a second psp_a row with the same reference and
// amount as a clean charge. The ledger has exactly one transaction for it.
func applyDuplicateCharge(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	row := target.File.Rows[target.RowIndex]
	duplicate := row
	duplicate.RecordID = "dup_" + row.RecordID
	duplicate.Memo = "synthetic duplicate charge"
	w.addPSPRow(target.File.Path, duplicate)
	scenario := w.failureScenario(DuplicateCharge, target.Txn,
		[]synth.Amount{amount(row.Currency, row.NetMinor)},
		[]synth.RecordRef{
			{Source: synth.SourcePSP, SourceRecordID: row.RecordID},
			{Source: synth.SourcePSP, SourceRecordID: duplicate.RecordID},
		},
		[]string{"duplicate"}, "duplicate", "T0",
		string(match.SideDuplicate), 0)
	w.addScenario(scenario)
	return nil
}

// applyPartialRefund reduces the psp_a refund by a positive delta. The ledger
// refund still has the original amount, and HD-3 allows either link or
// adjustment; the construction's intended path is adjustment.
func applyPartialRefund(w *World) error {
	target, err := w.nextTarget("refund")
	if err != nil {
		return err
	}
	row := &target.File.Rows[target.RowIndex]
	ledgerNet := row.NetMinor
	refundMinor := -ledgerNet
	if refundMinor <= 1 {
		return fmt.Errorf("%w: refund %s is too small to split", ErrInsufficientData, row.RecordID)
	}
	delta := refundMinor / 4
	if delta < 1 {
		delta = 1
	}
	row.NetMinor = ledgerNet + delta
	row.GrossMinor = row.NetMinor
	row.FeeMinor = 0
	scenario := w.failureScenario(PartialRefund, target.Txn,
		[]synth.Amount{amount(row.Currency, ledgerNet), amount(row.Currency, row.NetMinor)},
		target.Scenario.ExternalRecords,
		[]string{"link", "adjustment"}, "adjustment", "T0",
		string(match.SideAmountMismatch), delta)
	w.addScenario(scenario)
	return nil
}

// fxPair returns target, source currency. It prefers EUR -> USD and otherwise
// uses the first two sorted two-decimal currencies, so the rate is realistic
// for the default config and deterministic for custom ones.
func (w *World) fxPair() (string, string, error) {
	codes := w.CurrencyCodes()
	hasEUR, hasUSD := false, false
	for _, code := range codes {
		switch code {
		case "EUR":
			hasEUR = true
		case "USD":
			hasUSD = true
		}
	}
	if hasEUR && hasUSD {
		return "USD", "EUR", nil
	}
	var twoDecimal []string
	for _, code := range codes {
		currency, err := money.ParseCurrency(code)
		if err == nil && currency.Exponent() == 2 {
			twoDecimal = append(twoDecimal, code)
		}
	}
	if len(twoDecimal) < 2 {
		return "", "", fmt.Errorf("%w: fx_rounding needs two two-decimal currencies", ErrInvalidWorld)
	}
	return twoDecimal[1], twoDecimal[0], nil
}

// newConvertedCharge builds a balanced charge transaction for the converted
// amount. The clearing line carries the external reference and metadata so the
// ground truth and loader can connect it to the psp_a row.
func (w *World) newConvertedCharge(currency string, minor int64, reference, recordID string,
	at time.Time) (synth.Transaction, error) {
	lower := strings.ToLower(currency)
	clearing := "clearing_" + synth.SourcePSP + "_" + lower
	revenue := "revenue_" + lower
	clearingID, revenueID := w.accountID(clearing), w.accountID(revenue)
	if clearingID == "" || revenueID == "" {
		return synth.Transaction{}, fmt.Errorf("%w: accounts for %s are missing", ErrInvalidWorld, currency)
	}
	id := logicalID(w.Seed, "txn", "fx", recordID)
	txn := synth.Transaction{
		ID:             id,
		IdempotencyKey: "synth:" + id,
		Kind:           "charge",
		ValueDate:      at.UTC().Format(dateLayout),
		CreatedAt:      at.UTC().Format(timestampLayout),
		BookedAt:       at.UTC().Format(timestampLayout),
		Description:    "synthetic FX-converted charge " + recordID,
		Actor:          "system:synth",
		Lines: []synth.Line{
			{
				AccountID:   clearingID,
				AccountCode: clearing,
				Direction:   "debit",
				AmountMinor: minor,
				Currency:    currency,
				ExternalRef: reference,
				Metadata: map[string]any{
					"source":           synth.SourcePSP,
					"source_record_id": recordID,
				},
			},
			{
				AccountID:   revenueID,
				AccountCode: revenue,
				Direction:   "credit",
				AmountMinor: minor,
				Currency:    currency,
			},
		},
	}
	w.addTransaction(txn)
	return txn, nil
}

// sourceCutoff resolves the source's configured cutoff on the row's UTC
// business date. The sources map is the generator's configured calendar map, so
// late_settlement follows the same cutoff ingestion will use.
func sourceCutoff(sources map[string]config.SourceConfig, source string, at time.Time) (time.Time, error) {
	sourceConfig, ok := sources[source]
	if !ok {
		return time.Time{}, fmt.Errorf("%w: no calendar for source %q", ErrInvalidWorld, source)
	}
	location, err := time.LoadLocation(sourceConfig.Timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: source %s timezone %q: %v",
			ErrInvalidWorld, source, sourceConfig.Timezone, err)
	}
	parsed, err := time.Parse("15:04", sourceConfig.Cutoff)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: source %s cutoff %q: %v",
			ErrInvalidWorld, source, sourceConfig.Cutoff, err)
	}
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), parsed.Hour(), parsed.Minute(),
		0, 0, location).UTC(), nil
}
