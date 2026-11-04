package modes

import (
	"fmt"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/match"
	"github.com/TamasGorgics/recon-agent/synth"
)

// applyAmbiguous emits one psp_a charge and two ledger charges with the same
// amount and value date. Both ledger charges satisfy the fuzzy criteria, so the
// only correct ground truth is "do not guess": request_info or escalate.
func applyAmbiguous(w *World) error {
	paths := w.pspPaths()
	if len(paths) == 0 {
		return fmt.Errorf("%w: no psp_a file for ambiguous", ErrInvalidWorld)
	}
	variant := w.nextVariant()
	file := w.psp[paths[variant%len(paths)]]
	amountMinor := int64(750_000 + variant*211)
	recordID := fmt.Sprintf("psp_amb_%s_%s_%03d",
		file.compactDate(), strings.ToLower(file.Currency), variant)
	at := file.at(13, 30)
	settled := at
	w.addPSPRow(file.Path, pspRow{
		RecordID:   recordID,
		Type:       "charge",
		OccurredAt: at,
		SettledAt:  &settled,
		GrossMinor: amountMinor,
		NetMinor:   amountMinor,
		Currency:   file.Currency,
		Reference:  "REF-" + recordID,
		BatchID:    file.BatchID,
		Memo:       "synthetic ambiguous charge",
	})

	txns := make([]synth.Transaction, 0, ambiguityCandidateCount)
	for candidate := 1; candidate <= ambiguityCandidateCount; candidate++ {
		txn, err := w.newChargeTxn(file.Currency, amountMinor, file.date(),
			fmt.Sprintf("synthetic ambiguous ledger charge %s candidate %d", recordID, candidate),
			fmt.Sprintf("REF-AMB-%s-%d", recordID, candidate),
			"ambiguous", recordID, fmt.Sprintf("candidate-%d", candidate))
		if err != nil {
			return err
		}
		txns = append(txns, txn)
	}

	signature := match.BreakCandidate{
		ScopeID:         w.scopeID(synth.SourcePSP, file.Currency),
		Side:            match.SideExternalOnly,
		ExternalRecords: []match.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}},
		Currency:        file.Currency,
	}.Signature()
	w.addScenario(w.scenario(Ambiguous, []string{recordID}, nil,
		[]synth.RecordRef{{Source: synth.SourcePSP, SourceRecordID: recordID}},
		[]synth.Amount{amount(file.Currency, amountMinor)},
		accountsFor(txns[0]),
		[]string{"request_info", "escalate"}, "request_info", "T1", signature))
	return nil
}
