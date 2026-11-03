package modes_test

import (
	"errors"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/synth"
	"github.com/TamasGorgics/recon-agent/synth/modes"
)

// TestConfigValidation proves negative, unknown, and sum-mismatch mixes are
// typed configuration errors.
func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		config modes.Config
		field  string
	}{
		{
			name:   "negative total",
			config: modes.Config{Total: -1, Counts: map[string]int{modes.FeeVariance: -1}},
			field:  "total",
		},
		{
			name:   "zero total",
			config: modes.Config{Total: 0, Counts: map[string]int{}},
			field:  "total",
		},
		{
			name:   "negative count",
			config: modes.Config{Total: 5, Counts: map[string]int{modes.FeeVariance: -1}},
			field:  "counts.fee_variance",
		},
		{
			name:   "unknown mode",
			config: modes.Config{Total: 5, Counts: map[string]int{"not_a_mode": 5}},
			field:  "counts.not_a_mode",
		},
		{
			name:   "sum mismatch",
			config: modes.Config{Total: 5, Counts: map[string]int{modes.FeeVariance: 6}},
			field:  "counts",
		},
		{
			name:   "empty mix",
			config: modes.Config{Total: 5},
			field:  "counts",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Validate()
			var configErr *modes.ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("Validate() error = %v, want *modes.ConfigError", err)
			}
			if configErr.Field != testCase.field {
				t.Errorf("ConfigError.Field = %q, want %q", configErr.Field, testCase.field)
			}
			_, err = modes.Generate(clock.NewFake(mustTime(t, testNow)),
				testOptions(1, 1, synth.DifficultyEasy), testCase.config)
			if !errors.As(err, &configErr) {
				t.Errorf("Generate() error = %v, want *modes.ConfigError", err)
			}
		})
	}

	for _, valid := range []modes.Config{
		modes.DefaultConfig(6),
		modes.DefaultAllConfig(12),
		singleMode(modes.PartialRefund, 3),
		singleMode(modes.Ambiguous, 3),
	} {
		if err := valid.Validate(); err != nil {
			t.Errorf("valid config %+v: %v", valid, err)
		}
	}
}

// TestInsufficientDataIsTyped proves a mix larger than the clean world fails
// with a data error, not a panic or a silently short ground truth.
func TestInsufficientDataIsTyped(t *testing.T) {
	_, err := modes.Generate(clock.NewFake(mustTime(t, testNow)),
		testOptions(1, 1, synth.DifficultyEasy), singleMode(modes.MissingSettlement, 100))
	if !errors.Is(err, modes.ErrInsufficientData) {
		t.Fatalf("Generate() error = %v, want ErrInsufficientData", err)
	}
}
