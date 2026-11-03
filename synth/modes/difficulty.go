package modes

import (
	"fmt"
	"strings"

	"github.com/TamasGorgics/recon-agent/synth"
)

// GateDifficulty is the difficulty M1-16 must use for the milestone gate.
const GateDifficulty = synth.DifficultyRealistic

// Noise is the documented, difficulty-only noise knob set. Noise never changes
// a failure scenario; it only adds decoy external records after every scenario
// has been emitted. That is what makes "difficulty changes only documented
// noise knobs, not scenario correctness" testable.
type Noise struct {
	// UnrelatedBreaks is the number of external-only psp_a charge rows with no
	// ledger counterpart and no ground-truth scenario.
	UnrelatedBreaks int `json:"unrelated_breaks"`
	// NearDuplicateAmounts is the number of external-only psp_a charge rows
	// whose amount is within two minor units of an existing scenario amount.
	NearDuplicateAmounts int `json:"near_duplicate_amounts"`
}

// NoiseFor returns the documented noise knobs for a difficulty.
//
//	easy         no decoys
//	realistic    2 unrelated breaks, 2 near-duplicate amounts
//	adversarial  6 unrelated breaks, 6 near-duplicate amounts
func NoiseFor(difficulty string) (Noise, error) {
	switch difficulty {
	case synth.DifficultyEasy:
		return Noise{}, nil
	case synth.DifficultyRealistic:
		return Noise{UnrelatedBreaks: 2, NearDuplicateAmounts: 2}, nil
	case synth.DifficultyAdversarial:
		return Noise{UnrelatedBreaks: 6, NearDuplicateAmounts: 6}, nil
	default:
		return Noise{}, fmt.Errorf("%w: unknown difficulty %q", ErrInvalidWorld, difficulty)
	}
}

func (n Noise) canonical() string {
	return fmt.Sprintf("unrelated=%d;near_duplicate=%d", n.UnrelatedBreaks, n.NearDuplicateAmounts)
}

// applyNoise adds the difficulty decoys. It runs after every mode, so it can
// neither add nor remove a ground-truth scenario.
func applyNoise(w *World) error {
	noise, err := NoiseFor(w.Difficulty)
	if err != nil {
		return err
	}
	if noise.UnrelatedBreaks == 0 && noise.NearDuplicateAmounts == 0 {
		return nil
	}
	paths := w.pspPaths()
	if len(paths) == 0 {
		return fmt.Errorf("%w: no psp_a files for difficulty noise", ErrInvalidWorld)
	}
	for i := 0; i < noise.UnrelatedBreaks; i++ {
		file := w.psp[paths[i%len(paths)]]
		at := file.at(16, 0)
		recordID := fmt.Sprintf("noise_%s_%s_unrel_%02d",
			file.compactDate(), strings.ToLower(file.Currency), i)
		amountMinor := int64(4_100 + i*137)
		w.addPSPRow(file.Path, pspRow{
			RecordID:   recordID,
			Type:       "charge",
			OccurredAt: at,
			SettledAt:  &at,
			GrossMinor: amountMinor,
			NetMinor:   amountMinor,
			Currency:   file.Currency,
			Reference:  "NOISE-UNREL-" + recordID,
			BatchID:    file.BatchID,
			Memo:       "synthetic unrelated break",
		})
	}
	baseCurrency, baseAmount := w.noiseBase()
	for i := 0; i < noise.NearDuplicateAmounts; i++ {
		path := w.firstPSPPathForCurrency(baseCurrency)
		if path == "" {
			path = paths[0]
		}
		file := w.psp[path]
		delta := int64(1)
		if i%2 == 1 {
			delta = -1
		}
		amountMinor := baseAmount + delta
		if amountMinor <= 0 {
			amountMinor = baseAmount + 1
		}
		at := file.at(16, 15)
		recordID := fmt.Sprintf("noise_%s_%s_near_%02d",
			file.compactDate(), strings.ToLower(file.Currency), i)
		w.addPSPRow(file.Path, pspRow{
			RecordID:   recordID,
			Type:       "charge",
			OccurredAt: at,
			SettledAt:  &at,
			GrossMinor: amountMinor,
			NetMinor:   amountMinor,
			Currency:   file.Currency,
			Reference:  "NOISE-NEAR-" + recordID,
			BatchID:    file.BatchID,
			Memo:       "synthetic near-duplicate amount",
		})
	}
	return nil
}

// noiseBase picks a stable, positive amount and currency from the emitted
// scenarios so a near-duplicate is genuinely near an existing amount.
func (w *World) noiseBase() (string, int64) {
	for _, scenario := range w.Truth {
		for _, amount := range scenario.Amounts {
			if amount.AmountMinor > 2 {
				return amount.Currency, amount.AmountMinor
			}
		}
	}
	return "USD", 5_000
}
