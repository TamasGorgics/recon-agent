package report

import (
	"math"
	"testing"
)

func TestWilsonReferenceValues(t *testing.T) {
	cases := []struct {
		name        string
		numerator   int
		denominator int
		low         float64
		high        float64
		tolerance   float64
	}{
		{name: "0 of 30", numerator: 0, denominator: 30, low: 0, high: 0.11351339317396876, tolerance: 1e-9},
		{name: "30 of 30", numerator: 30, denominator: 30, low: 0.8864866068260312, high: 1, tolerance: 1e-9},
		{name: "15 of 30", numerator: 15, denominator: 30, low: 0.33154125640533766, high: 0.6684587435946623, tolerance: 1e-9},
		{name: "0 of 1", numerator: 0, denominator: 1, low: 0, high: 0.7934506856227626, tolerance: 1e-9},
		{name: "1 of 1", numerator: 1, denominator: 1, low: 0.20654931437723745, high: 1, tolerance: 1e-9},
		{name: "29 of 30", numerator: 29, denominator: 30, low: 0.8332960900859082, high: 0.9940914096183874, tolerance: 1e-9},
		{name: "about 0 of 30", numerator: 0, denominator: 30, low: 0, high: 0.1135, tolerance: 1e-4},
		{name: "about 30 of 30", numerator: 30, denominator: 30, low: 0.8865, high: 1, tolerance: 1e-4},
		{name: "about 15 of 30", numerator: 15, denominator: 30, low: 0.3315, high: 0.6685, tolerance: 1e-4},
		{name: "about 0 of 1", numerator: 0, denominator: 1, low: 0, high: 0.7934, tolerance: 1e-4},
		{name: "about 1 of 1", numerator: 1, denominator: 1, low: 0.2065, high: 1, tolerance: 1e-4},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			interval, ok := Wilson(testCase.numerator, testCase.denominator)
			if !ok {
				t.Fatal("Wilson returned undefined")
			}
			if math.Abs(interval.Low-testCase.low) > testCase.tolerance {
				t.Fatalf("low = %.15f, want %.15f (±%g)", interval.Low, testCase.low, testCase.tolerance)
			}
			if math.Abs(interval.High-testCase.high) > testCase.tolerance {
				t.Fatalf("high = %.15f, want %.15f (±%g)", interval.High, testCase.high, testCase.tolerance)
			}
			if interval.Low < 0 || interval.Low > 1 || interval.High < 0 || interval.High > 1 {
				t.Fatalf("interval outside [0,1]: %+v", interval)
			}
			if interval.Low > interval.High {
				t.Fatalf("interval inverted: %+v", interval)
			}
		})
	}
}

func TestWilsonZeroDenominatorUndefined(t *testing.T) {
	for _, denominator := range []int{0, -1} {
		if _, ok := Wilson(0, denominator); ok {
			t.Fatalf("Wilson(0, %d) = defined, want undefined", denominator)
		}
	}
}

func TestWilsonClampsOutOfRangeNumerator(t *testing.T) {
	interval, ok := Wilson(31, 30)
	if !ok {
		t.Fatal("Wilson returned undefined")
	}
	if math.Abs(interval.High-1) > 1e-12 || interval.Low < 0.8 || interval.Low > 1 {
		t.Fatalf("clamped interval = %+v", interval)
	}
	interval, ok = Wilson(-1, 30)
	if !ok {
		t.Fatal("Wilson returned undefined")
	}
	if interval.Low != 0 || interval.High > 0.2 {
		t.Fatalf("clamped interval = %+v", interval)
	}
}

func TestMeanSpread(t *testing.T) {
	mean, minimum, maximum, stddev := meanSpread([]float64{0.5, 0.75, 0.75})
	if math.Abs(mean-2.0/3.0) > 1e-12 {
		t.Fatalf("mean = %v", mean)
	}
	if minimum != 0.5 || maximum != 0.75 {
		t.Fatalf("min/max = %v/%v", minimum, maximum)
	}
	if stddev == nil || math.Abs(*stddev-0.14433756729740643) > 1e-12 {
		t.Fatalf("stddev = %v", stddev)
	}
	mean, minimum, maximum, stddev = meanSpread([]float64{0.4})
	if mean != 0.4 || minimum != 0.4 || maximum != 0.4 || stddev != nil {
		t.Fatalf("single value = %v/%v/%v/%v", mean, minimum, maximum, stddev)
	}
}
