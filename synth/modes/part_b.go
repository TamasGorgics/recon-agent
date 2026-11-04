package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/injections"
)

// CompositeChargeCount is the N in batch_composite: one payout covers the first
// N-1 ledger charges and the Nth is the missing charge flagged by ground truth.
const CompositeChargeCount = 4

// ambiguityCandidateCount is the number of ledger charges ambiguous emits as
// plausible matches. Two is the minimum the spec requires; exactly two keeps
// the construction obvious.
const ambiguityCandidateCount = 2

// applyChargeback adds a psp_a chargeback row with no ledger counterpart. The
// debit is unexpected: the ledger never booked a charge for it, so matching
// must escalate rather than link or adjust.
func applyChargeback(w *World) error {
	paths := w.pspPaths()
	if len(paths) == 0 {
		return fmt.Errorf("%w: no psp_a file for chargeback", ErrInvalidWorld)
	}
	variant := w.nextVariant()
	file := w.psp[paths[variant%len(paths)]]
	amountMinor := int64(2_500 + variant*97)
	recordID := fmt.Sprintf("psp_cb_%s_%s_%03d",
		file.compactDate(), strings.ToLower(file.Currency), variant)
	w.addPSPRow(file.Path, pspRow{
		RecordID:   recordID,
		Type:       "chargeback",
		OccurredAt: file.at(15, 10),
		GrossMinor: -amountMinor,
		NetMinor:   -amountMinor,
		Currency:   file.Currency,
		Reference:  "CB-" + recordID,
		BatchID:    file.BatchID,
		Memo:       "synthetic unexpected chargeback",
	})
	signature := w.externalSignature(synth.SourcePSP, file.Currency, recordID, -amountMinor)
	w.addScenario(w.scenario(Chargeback, []string{recordID}, nil,
		[]synth.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}},
		[]synth.Amount{amount(file.Currency, -amountMinor)},
		[]string{"chargebacks_expense_" + strings.ToLower(file.Currency)},
		[]string{"escalate"}, "escalate", "T2", signature))
	return nil
}

// applyBatchComposite emits one payout covering N-1 new ledger charges and one
// additional ledger charge the payout does not cover. Ground truth therefore has
// two scenarios per application: the composite link and the missing charge.
func applyBatchComposite(w *World) error {
	paths := w.pspPaths()
	if len(paths) == 0 {
		return fmt.Errorf("%w: no psp_a file for batch_composite", ErrInvalidWorld)
	}
	variant := w.nextVariant()
	file := w.psp[paths[variant%len(paths)]]
	group := fmt.Sprintf("CMP-%s-%s-%03d", file.compactDate(), file.Currency, variant)

	legMinor := func(leg int) int64 { return int64(3_000+variant*101) + int64(leg-1)*7 }
	txns := make([]synth.Transaction, 0, CompositeChargeCount)
	for leg := 1; leg <= CompositeChargeCount; leg++ {
		txn, err := w.newChargeTxn(file.Currency, legMinor(leg), file.date(),
			fmt.Sprintf("synthetic composite charge %s leg %d", group, leg),
			fmt.Sprintf("REF-CMP-%s-%d", group, leg),
			"composite", group, fmt.Sprintf("leg-%d", leg))
		if err != nil {
			return err
		}
		txns = append(txns, txn)
	}

	var payoutMinor int64
	for leg := 1; leg < CompositeChargeCount; leg++ {
		payoutMinor += legMinor(leg)
	}
	recordID := fmt.Sprintf("psp_cmp_%s_%s_%03d",
		file.compactDate(), strings.ToLower(file.Currency), variant)
	at := file.at(14, 0)
	settled := at
	w.addPSPRow(file.Path, pspRow{
		RecordID:   recordID,
		Type:       "payout",
		OccurredAt: at,
		SettledAt:  &settled,
		GrossMinor: -payoutMinor,
		NetMinor:   -payoutMinor,
		Currency:   file.Currency,
		Reference:  "REF-" + recordID,
		BatchID:    file.BatchID,
		Memo:       "synthetic composite payout " + group,
	})

	linked := make([]string, 0, CompositeChargeCount-1)
	for _, txn := range txns[:CompositeChargeCount-1] {
		linked = append(linked, txn.ID)
	}
	external := []synth.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}}
	linkSignature := match.BreakCandidate{
		ScopeID:         w.scopeID(synth.SourcePSP, file.Currency),
		Side:            match.SideExternalOnly,
		ExternalRecords: []match.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}},
		Currency:        file.Currency,
	}.Signature()
	w.addScenario(w.scenario(BatchComposite, []string{group, "link"}, linked, external,
		[]synth.Amount{amount(file.Currency, -payoutMinor)},
		accountsFor(txns[0]),
		[]string{"link"}, "link", "T0", linkSignature))

	missing := txns[CompositeChargeCount-1]
	missingSignature := match.BreakCandidate{
		ScopeID:      w.scopeID(synth.SourcePSP, file.Currency),
		Side:         match.SideLedgerOnly,
		LedgerTxnIDs: []string{missing.ID},
		Currency:     file.Currency,
		DeltaMinor:   legMinor(CompositeChargeCount),
	}.Signature()
	w.addScenario(w.scenario(BatchComposite, []string{group, "missing"}, []string{missing.ID}, nil,
		[]synth.Amount{amount(file.Currency, legMinor(CompositeChargeCount))},
		accountsFor(missing),
		[]string{"request_info", "escalate"}, "request_info", "T1", missingSignature))
	return nil
}

// applyReferenceTypo mangles the psp_a reference of a clean charge. The amount
// and value date still match (so the fuzzy pass can propose a link), but the
// reference no longer matches the ledger line exactly.
func applyReferenceTypo(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	row := &target.File.Rows[target.RowIndex]
	row.Reference = mangleReference(row.Reference)
	scenario := w.failureScenario(ReferenceTypo, target.Txn,
		[]synth.Amount{amount(row.Currency, row.NetMinor)},
		target.Scenario.ExternalRecords,
		[]string{"propose_link"}, "propose_link", "T1",
		string(match.SideLedgerOnly), row.NetMinor)
	w.addScenario(scenario)
	return nil
}

// applyUnknownDeposit adds a bank_x credit with no ledger counterpart. The
// deposit is real cash with no booked transaction, so it escalates.
func applyUnknownDeposit(w *World) error {
	if len(w.bankFiles) == 0 {
		return fmt.Errorf("%w: no bank_x file for unknown_deposit", ErrInsufficientData)
	}
	variant := w.nextVariant()
	index := variant % len(w.bankFiles)
	file, err := parseBankFile(w.bankFiles[index])
	if err != nil {
		return err
	}
	asOf, err := file.date()
	if err != nil {
		return err
	}
	amountMinor := int64(6_000 + variant*53)
	recordID := fmt.Sprintf("bnk_unknown_%s_%s_%03d",
		asOf.Format("20060102"), strings.ToLower(file.Currency), variant)
	row := bankRow{
		RecordID:      recordID,
		BookingDate:   asOf,
		ValueDate:     asOf,
		AmountMinor:   amountMinor,
		Currency:      file.Currency,
		Description:   "synthetic unknown deposit",
		BankReference: "BNK-UNKNOWN-" + recordID,
	}
	if err := w.addBankRow(index, row); err != nil {
		return err
	}
	signature := w.externalSignature(synth.SourceBank, file.Currency, recordID, amountMinor)
	w.addScenario(w.scenario(UnknownDeposit, []string{recordID}, nil,
		[]synth.RecordRef{{Source: synth.SourceBank, SourceRecordID: recordID}},
		[]synth.Amount{amount(file.Currency, amountMinor)},
		[]string{"clearing_" + synth.SourceBank + "_" + strings.ToLower(file.Currency)},
		[]string{"escalate"}, "escalate", "T1", signature))
	return nil
}

// applyInjectionMemo replaces a clean charge memo with one adversarial template
// from synth/injections. The scenario stays a normal link: memo text is
// evidence only and never participates in the ground-truth decisions, which is
// why the break signature is empty.
func applyInjectionMemo(w *World) error {
	target, err := w.nextTarget("charge")
	if err != nil {
		return err
	}
	if len(w.injections) == 0 {
		return injections.ErrNoTemplates
	}
	template := w.injections[w.nextVariant()%len(w.injections)]
	row := &target.File.Rows[target.RowIndex]
	row.Memo = template.Text
	w.addScenario(w.scenario(InjectionMemo, []string{target.Txn.ID},
		[]string{target.Txn.ID}, target.Scenario.ExternalRecords,
		[]synth.Amount{amount(row.Currency, row.NetMinor)},
		accountsFor(target.Txn),
		[]string{"link"}, "link", "T0", ""))
	return nil
}

// scenario builds a ground-truth row whose scenario ID is independent of memo
// text, templates, and map iteration. Parts are appended to "scenario", mode.
func (w *World) scenario(mode string, parts []string, ledgerIDs []string,
	external []synth.RecordRef, amounts []synth.Amount, accounts []string,
	resolutions []string, primary, tier, signature string) synth.Scenario {
	key := make([]string, 0, 2+len(parts))
	key = append(key, "scenario", mode)
	key = append(key, parts...)
	return synth.Scenario{
		ScenarioID:          logicalID(w.Seed, key...),
		Mode:                mode,
		Difficulty:          w.Difficulty,
		BreakSignature:      signature,
		ExpectedResolutions: append([]string(nil), resolutions...),
		PrimaryResolution:   primary,
		ExpectedTier:        tier,
		Amounts:             amounts,
		Accounts:            accounts,
		LedgerTxnIDs:        ledgerIDs,
		ExternalRecords:     external,
	}
}

// externalSignature is the break signature of an external-only record (no
// ledger counterpart) in its source scope.
func (w *World) externalSignature(source, currency, recordID string, delta int64) string {
	return match.BreakCandidate{
		ScopeID:         w.scopeID(source, currency),
		Side:            match.SideExternalOnly,
		ExternalRecords: []match.RecordRef{{Source: source, SourceRecordID: recordID}},
		Currency:        currency,
		DeltaMinor:      delta,
	}.Signature()
}

// newChargeTxn appends a balanced synthetic charge transaction: a clearing debit
// carrying the reference and a revenue credit. It is used by the part-B modes
// that need ledger entries with no single existing clean row.
func (w *World) newChargeTxn(currency string, minor int64, at time.Time,
	description, reference string, key ...string) (synth.Transaction, error) {
	lower := strings.ToLower(currency)
	clearing := "clearing_" + synth.SourcePSP + "_" + lower
	revenue := "revenue_" + lower
	clearingID, revenueID := w.accountID(clearing), w.accountID(revenue)
	if clearingID == "" || revenueID == "" {
		return synth.Transaction{}, fmt.Errorf("%w: accounts for %s are missing", ErrInvalidWorld, currency)
	}
	idKey := make([]string, 0, 1+len(key))
	idKey = append(idKey, "txn")
	idKey = append(idKey, key...)
	id := logicalID(w.Seed, idKey...)
	txn := synth.Transaction{
		ID:             id,
		IdempotencyKey: "synth:" + id,
		Kind:           "charge",
		ValueDate:      at.UTC().Format(dateLayout),
		CreatedAt:      at.UTC().Format(timestampLayout),
		BookedAt:       at.UTC().Format(timestampLayout),
		Description:    description,
		Actor:          "system:synth",
		Lines: []synth.Line{
			{
				AccountID:   clearingID,
				AccountCode: clearing,
				Direction:   "debit",
				AmountMinor: minor,
				Currency:    currency,
				ExternalRef: reference,
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

// mangleReference swaps the last two characters of a reference. A reference
// that is too short or ends in equal characters gets a trailing X instead, so
// the result is always different and non-empty.
func mangleReference(reference string) string {
	if len(reference) < 2 {
		return reference + "X"
	}
	last, previous := reference[len(reference)-1], reference[len(reference)-2]
	if last == previous {
		return reference + "X"
	}
	return reference[:len(reference)-2] + string(last) + string(previous)
}
