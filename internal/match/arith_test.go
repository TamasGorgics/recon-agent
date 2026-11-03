package match

import (
	"math"
	"testing"
)

// TestCheckedArithmetic covers the int64 boundary cases the matcher relies on.
// Every overflow must be reported, never wrapped.
func TestCheckedArithmetic(t *testing.T) {
	addTests := []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{"max plus zero", math.MaxInt64, 0, math.MaxInt64, true},
		{"max plus one", math.MaxInt64, 1, 0, false},
		{"min plus minus one", math.MinInt64, -1, 0, false},
		{"min plus one", math.MinInt64, 1, math.MinInt64 + 1, true},
		{"min plus max", math.MinInt64, math.MaxInt64, -1, true},
		{"max plus min", math.MaxInt64, math.MinInt64, -1, true},
	}
	for _, tc := range addTests {
		t.Run("add/"+tc.name, func(t *testing.T) {
			got, ok := addInt64(tc.a, tc.b)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("addInt64(%d, %d) = (%d, %t), want (%d, %t)", tc.a, tc.b, got, ok, tc.want, tc.ok)
			}
		})
	}

	subTests := []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{"max minus max", math.MaxInt64, math.MaxInt64, 0, true},
		{"min minus min", math.MinInt64, math.MinInt64, 0, true},
		{"max minus min", math.MaxInt64, math.MinInt64, 0, false},
		{"min minus max", math.MinInt64, math.MaxInt64, 0, false},
		{"min minus minus one", math.MinInt64, -1, math.MinInt64 + 1, true},
		{"one minus min", 1, math.MinInt64, 0, false},
		{"zero minus min", 0, math.MinInt64, 0, false},
		{"max minus negative max", math.MaxInt64, -math.MaxInt64, 0, false},
	}
	for _, tc := range subTests {
		t.Run("sub/"+tc.name, func(t *testing.T) {
			got, ok := subInt64(tc.a, tc.b)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("subInt64(%d, %d) = (%d, %t), want (%d, %t)", tc.a, tc.b, got, ok, tc.want, tc.ok)
			}
		})
	}

	absTests := []struct {
		name string
		in   int64
		want int64
		ok   bool
	}{
		{"zero", 0, 0, true},
		{"max", math.MaxInt64, math.MaxInt64, true},
		{"minus one", -1, 1, true},
		{"min plus one", math.MinInt64 + 1, math.MaxInt64, true},
		{"min", math.MinInt64, 0, false},
	}
	for _, tc := range absTests {
		t.Run("abs/"+tc.name, func(t *testing.T) {
			got, ok := absInt64(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("absInt64(%d) = (%d, %t), want (%d, %t)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}

	deltaTests := []struct {
		name string
		a, b int64
		want int64
		ok   bool
	}{
		{"equal", math.MaxInt64, math.MaxInt64, 0, true},
		{"in range", math.MaxInt64, math.MaxInt64 - 100, 100, true},
		{"min plus one vs min", math.MinInt64 + 1, math.MinInt64, 1, true},
		{"one vs min", 1, math.MinInt64, 0, false},
		{"min vs zero", math.MinInt64, 0, 0, false},
		{"max vs minus one", math.MaxInt64, -1, 0, false},
	}
	for _, tc := range deltaTests {
		t.Run("delta/"+tc.name, func(t *testing.T) {
			got, ok := absDelta(tc.a, tc.b)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("absDelta(%d, %d) = (%d, %t), want (%d, %t)", tc.a, tc.b, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestMulDivInt64NoIntermediateOverflow covers the weighted fuzzy score: a
// tolerance near MaxInt64 still produces an exact quotient via 128-bit math.
func TestMulDivInt64NoIntermediateOverflow(t *testing.T) {
	tests := []struct {
		name    string
		a, b, c int64
		want    int64
		ok      bool
	}{
		{"zero numerator", 0, math.MaxInt64, math.MaxInt64, 0, true},
		{"max weighted", int64(fuzzyAmountWeightBPS), math.MaxInt64 - 1, math.MaxInt64, 4999, true},
		{"full weight", int64(fuzzyAmountWeightBPS), math.MaxInt64, math.MaxInt64, int64(fuzzyAmountWeightBPS), true},
		{"quotient overflow", math.MaxInt64, math.MaxInt64, 1, 0, false},
		{"zero divisor", 1, 1, 0, 0, false},
		{"negative numerator", -1, 1, 1, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mulDivInt64(tc.a, tc.b, tc.c)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("mulDivInt64(%d, %d, %d) = (%d, %t), want (%d, %t)",
					tc.a, tc.b, tc.c, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestFuzzyScoreBPSHugeTolerance proves the fuzzy weighting does not wrap for
// a valid but extreme tolerance; wrapped scores could otherwise fabricate a
// match (or hide one).
func TestFuzzyScoreBPSHugeTolerance(t *testing.T) {
	tests := []struct {
		name                 string
		delta, tolerance     int64
		days, window         int
		recordRef, ledgerRef string
		want                 int
		ok                   bool
	}{
		{"exact amount huge tolerance", 0, math.MaxInt64, 0, 1, "R", "R", 10_000, true},
		{"one minor unit huge tolerance", 1, math.MaxInt64, 0, 1, "R", "R", 9_999, true},
		{"small tolerance in range", 2, 100, 0, 0, "", "", 7_900, true},
		{"delta equals tolerance", math.MaxInt64, math.MaxInt64, 0, 0, "", "", 3_000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := fuzzyScoreBPS(tc.delta, tc.tolerance, tc.days, tc.window, tc.recordRef, tc.ledgerRef)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("fuzzyScoreBPS = (%d, %t), want (%d, %t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestSubsetSumSuffixOverflowKeepsExactSolution proves an overflowing suffix
// sum is treated as an unbounded upper bound, not as a wrapped prune value
// that would hide the [3,4] subset summing to 7.
func TestSubsetSumSuffixOverflowKeepsExactSolution(t *testing.T) {
	solutions := subsetSum([]int64{math.MaxInt64, 3, 4}, 7, 8, 1_000)
	if len(solutions) != 1 {
		t.Fatalf("solutions = %v, want exactly one", solutions)
	}
	got := solutions[0]
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("solution = %v, want indexes [1 2]", got)
	}
}

// TestSubsetSumNodeCapReturnsNoSolution proves a truncated search never returns
// the solutions it found before the budget ran out. With a full budget the same
// amounts are ambiguous (60+40 and 50+50); with a four-node budget only 60+40 is
// discovered, so returning it would be a partial match.
func TestSubsetSumNodeCapReturnsNoSolution(t *testing.T) {
	amounts := []int64{60, 40, 50, 50}
	full := subsetSum(amounts, 100, 8, 1_000)
	if len(full) != 2 {
		t.Fatalf("full search solutions = %v, want two (the case is ambiguous)", full)
	}
	if capped := subsetSum(amounts, 100, 8, 4); capped != nil {
		t.Fatalf("capped search returned partial solutions %v, want nil", capped)
	}
}
