package verify

import (
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestRationaleIndependence proves the evaluator API has no rationale,
// confidence, or free-text input channel, and that repeated calls over a
// variety of untrusted rationale/confidence-like values produce byte-identical
// results. The strings are deliberately not passed to the evaluators; this
// guards against an accidental future coupling.
func TestRationaleIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(20261113))
	baseline := map[string]string{
		"fee":       resultJSON(t, VerifyFeeVariance(validFeeInput(), DefaultOptions())),
		"fx":        resultJSON(t, VerifyFXRounding(validFXInput(), DefaultOptions())),
		"rounding":  resultJSON(t, VerifyRounding(validRoundingInput(), DefaultOptions())),
		"reference": resultJSON(t, VerifyReferenceTypo(validReferenceInput(), DefaultOptions())),
	}
	for i := 0; i < 250; i++ {
		rationale := randomText(rng, 1+rng.Intn(80))
		confidence := rng.Intn(101)
		_ = rationale
		_ = confidence
		if got := resultJSON(t, VerifyFeeVariance(validFeeInput(), DefaultOptions())); got != baseline["fee"] {
			t.Fatalf("fee result changed with rationale/confidence: %s != %s", got, baseline["fee"])
		}
		if got := resultJSON(t, VerifyFXRounding(validFXInput(), DefaultOptions())); got != baseline["fx"] {
			t.Fatalf("fx result changed with rationale/confidence: %s != %s", got, baseline["fx"])
		}
		if got := resultJSON(t, VerifyRounding(validRoundingInput(), DefaultOptions())); got != baseline["rounding"] {
			t.Fatalf("rounding result changed with rationale/confidence: %s != %s", got, baseline["rounding"])
		}
		if got := resultJSON(t, VerifyReferenceTypo(validReferenceInput(), DefaultOptions())); got != baseline["reference"] {
			t.Fatalf("reference result changed with rationale/confidence: %s != %s", got, baseline["reference"])
		}
	}
}

func TestInputsHaveNoRationaleOrConfidenceFields(t *testing.T) {
	inputTypes := []reflect.Type{
		reflect.TypeOf(FeeVarianceInput{}),
		reflect.TypeOf(FXRoundingInput{}),
		reflect.TypeOf(RoundingInput{}),
		reflect.TypeOf(ReferenceTypoInput{}),
	}
	for _, inputType := range inputTypes {
		for i := 0; i < inputType.NumField(); i++ {
			name := strings.ToLower(inputType.Field(i).Name)
			if strings.Contains(name, "rationale") || strings.Contains(name, "confidence") {
				t.Fatalf("%s exposes an untrusted %s field", inputType, inputType.Field(i).Name)
			}
		}
	}
}

func TestConcurrentEvaluators(t *testing.T) {
	fee := validFeeInput()
	fx := validFXInput()
	rounding := validRoundingInput()
	reference := validReferenceInput()
	opts := Options{
		RoundingCapMinor:    map[string]int64{"USD": 1, "EUR": 1},
		ReferenceWindowDays: 3,
	}
	const workers = 32
	const iterations = 50

	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for i := 0; i < iterations; i++ {
				if result := VerifyFeeVariance(fee, opts); !result.Passed {
					t.Errorf("concurrent fee failed: %+v", result.Details)
					return
				}
				if result := VerifyFXRounding(fx, opts); !result.Passed {
					t.Errorf("concurrent fx failed: %+v", result.Details)
					return
				}
				if result := VerifyRounding(rounding, opts); !result.Passed {
					t.Errorf("concurrent rounding failed: %+v", result.Details)
					return
				}
				if result := VerifyReferenceTypo(reference, opts); !result.Passed {
					t.Errorf("concurrent reference failed: %+v", result.Details)
					return
				}
			}
		}()
	}
	wait.Wait()

	if len(opts.RoundingCapMinor) != 2 || opts.RoundingCapMinor["USD"] != 1 || opts.RoundingCapMinor["EUR"] != 1 {
		t.Fatalf("evaluators mutated shared Options: %#v", opts.RoundingCapMinor)
	}
}

func randomText(rng *rand.Rand, length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789<>{}\"'\\\n\t"
	var builder strings.Builder
	for i := 0; i < length; i++ {
		builder.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return builder.String()
}
