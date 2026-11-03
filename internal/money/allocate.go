package money

import (
	"fmt"
	"math/big"
)

// Allocate splits m into len(weights) parts proportional to weights. Each
// part is truncated towards zero and the whole residual is added to
// parts[residualTo], so the parts always sum back to m exactly. Negative
// weights, an all-zero weight total, an empty weight slice, and a residual
// index outside the result are rejected with ErrInvalidAllocation.
func Allocate(m Money, weights []int64, residualTo int) ([]Money, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	if len(weights) == 0 {
		return nil, fmt.Errorf("%w: no weights", ErrInvalidAllocation)
	}
	if residualTo < 0 || residualTo >= len(weights) {
		return nil, fmt.Errorf("%w: residual index %d outside [0, %d)", ErrInvalidAllocation, residualTo, len(weights))
	}

	totalWeight := new(big.Int)
	for i, weight := range weights {
		if weight < 0 {
			return nil, fmt.Errorf("%w: weight %d at index %d is negative", ErrInvalidAllocation, weight, i)
		}
		totalWeight.Add(totalWeight, big.NewInt(weight))
	}
	if totalWeight.Sign() == 0 {
		return nil, fmt.Errorf("%w: weights sum to zero", ErrInvalidAllocation)
	}

	amount := big.NewInt(m.amount)
	parts := make([]Money, len(weights))
	allocated := new(big.Int)
	for i, weight := range weights {
		product := new(big.Int).Mul(amount, big.NewInt(weight))
		part := new(big.Int).Quo(product, totalWeight)
		if !part.IsInt64() {
			return nil, fmt.Errorf("%w: allocated part %s does not fit in int64", ErrOverflow, part)
		}
		parts[i] = FromMinor(part.Int64(), m.currency)
		allocated.Add(allocated, part)
	}

	residual := new(big.Int).Sub(amount, allocated)
	if !residual.IsInt64() {
		return nil, fmt.Errorf("%w: residual %s does not fit in int64", ErrOverflow, residual)
	}
	adjusted := new(big.Int).Add(big.NewInt(parts[residualTo].amount), residual)
	if !adjusted.IsInt64() {
		return nil, fmt.Errorf("%w: adjusted part %s does not fit in int64", ErrOverflow, adjusted)
	}
	parts[residualTo] = FromMinor(adjusted.Int64(), m.currency)
	return parts, nil
}
