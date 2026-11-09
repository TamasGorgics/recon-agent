package report

import "math"

// z95 is the two-sided 95% normal quantile used by the Wilson score interval.
// It is a documented constant: the report has no configurable confidence
// level (spec §5.4).
const z95 = 1.959963984540054

// Interval is a closed Wilson score interval. Low and High are proportions in
// [0, 1].
type Interval struct {
	Low  float64
	High float64
}

// Wilson returns the Wilson score interval for numerator successes out of
// denominator trials. A zero denominator is undefined: ok is false and the
// zero Interval must not be rendered. The interval is clamped to [0, 1] for
// floating-point cleanup only.
func Wilson(numerator, denominator int) (Interval, bool) {
	if denominator <= 0 {
		return Interval{}, false
	}
	if numerator < 0 {
		numerator = 0
	}
	if numerator > denominator {
		numerator = denominator
	}
	n := float64(denominator)
	x := float64(numerator)
	p := x / n
	z2 := z95 * z95
	denominatorTerm := 1 + z2/n
	center := (p + z2/(2*n)) / denominatorTerm
	half := z95 / denominatorTerm * math.Sqrt(p*(1-p)/n+z2/(4*n*n))
	low := clamp01(center - half)
	high := clamp01(center + half)
	return Interval{Low: low, High: high}, true
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// meanSpread returns the arithmetic mean, minimum, maximum and sample standard
// deviation of values. The standard deviation uses the k-1 denominator and is
// nil for fewer than two values (a single repeat has no spread).
func meanSpread(values []float64) (mean, minValue, maxValue float64, stddev *float64) {
	if len(values) == 0 {
		return 0, 0, 0, nil
	}
	minValue = values[0]
	maxValue = values[0]
	sum := 0.0
	for _, value := range values {
		sum += value
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	mean = sum / float64(len(values))
	if len(values) < 2 {
		return mean, minValue, maxValue, nil
	}
	sumSquares := 0.0
	for _, value := range values {
		delta := value - mean
		sumSquares += delta * delta
	}
	sd := math.Sqrt(sumSquares / float64(len(values)-1))
	return mean, minValue, maxValue, &sd
}
