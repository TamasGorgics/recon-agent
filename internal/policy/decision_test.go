package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldenConfigHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDecisionValidate(t *testing.T) {
	valid := Decision{
		SchemaVersion: 1,
		Tier:          TierT1,
		Rule:          CodeVerifierMissing,
		Reasons:       []string{CodeVerifierMissing},
		ConfigHash:    goldenConfigHash,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid decision rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Decision)
	}{
		{"schema version 0", func(d *Decision) { d.SchemaVersion = 0 }},
		{"schema version 2", func(d *Decision) { d.SchemaVersion = 2 }},
		{"unknown tier", func(d *Decision) { d.Tier = "T3" }},
		{"empty tier", func(d *Decision) { d.Tier = "" }},
		{"unknown rule", func(d *Decision) { d.Rule = "made_up" }},
		{"empty rule", func(d *Decision) { d.Rule = "" }},
		{"nil reasons", func(d *Decision) { d.Reasons = nil }},
		{"empty reasons", func(d *Decision) { d.Reasons = []string{} }},
		{"unknown reason", func(d *Decision) { d.Reasons = []string{"made_up"} }},
		{"duplicate reason", func(d *Decision) { d.Reasons = []string{CodeVerifierMissing, CodeVerifierMissing} }},
		{"rule not in reasons", func(d *Decision) { d.Reasons = []string{CodeVerifierFailed} }},
		{"T2 rule on T0", func(d *Decision) {
			d.Tier, d.Rule, d.Reasons = TierT0, CodeHighValue, []string{CodeHighValue}
		}},
		{"T2 rule on T1", func(d *Decision) {
			d.Tier, d.Rule, d.Reasons = TierT1, CodeCrossCurrency, []string{CodeCrossCurrency}
		}},
		{"T0 auto_apply rule on T1", func(d *Decision) {
			d.Tier, d.Rule, d.Reasons = TierT1, CodeAutoApply, []string{CodeAutoApply}
		}},
		{"T1 gate rule on T2", func(d *Decision) {
			d.Tier, d.Rule, d.Reasons = TierT2, CodeVerifierMissing, []string{CodeVerifierMissing}
		}},
		{"T0 value gate rule", func(d *Decision) {
			d.Tier, d.Rule, d.Reasons = TierT0, CodeValueCap, []string{CodeValueCap}
		}},
		{"uppercase hash", func(d *Decision) { d.ConfigHash = strings.ToUpper(goldenConfigHash) }},
		{"short hash", func(d *Decision) { d.ConfigHash = "abc123" }},
		{"empty hash", func(d *Decision) { d.ConfigHash = "" }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			d.Reasons = append([]string(nil), valid.Reasons...)
			tc.edit(&d)
			if err := d.Validate(); !errors.Is(err, ErrInvalidDecision) {
				t.Fatalf("Validate error = %v, want ErrInvalidDecision", err)
			}
			if _, err := d.JSON(); !errors.Is(err, ErrInvalidDecision) {
				t.Fatalf("JSON error = %v, want ErrInvalidDecision", err)
			}
		})
	}
}

// TestDecisionValidateTierRules pins the tier/rule/reasons consistency that the
// engine emits and rejects forged combinations.
func TestDecisionValidateTierRules(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		wantErr  bool
	}{
		{
			name: "T0 auto_apply",
			decision: Decision{SchemaVersion: 1, Tier: TierT0, Rule: CodeAutoApply,
				Reasons: []string{CodeAutoApply}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T1 gate",
			decision: Decision{SchemaVersion: 1, Tier: TierT1, Rule: CodeValueCap,
				Reasons: []string{CodeValueCap}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T1 default single approval",
			decision: Decision{SchemaVersion: 1, Tier: TierT1, Rule: CodeDefaultSingleApproval,
				Reasons: []string{CodeDefaultSingleApproval}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T1 confidence downgrade",
			decision: Decision{SchemaVersion: 1, Tier: TierT1, Rule: CodeConfidenceDowngrade,
				Reasons: []string{CodeAutoApply, CodeConfidenceDowngrade}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T2 predicate with multiple reasons",
			decision: Decision{SchemaVersion: 1, Tier: TierT2, Rule: CodeHighValue,
				Reasons: []string{CodeHighValue, CodeCrossCurrency}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T2 confidence downgrade",
			decision: Decision{SchemaVersion: 1, Tier: TierT2, Rule: CodeConfidenceDowngrade,
				Reasons: []string{CodeVerifierMissing, CodeConfidenceDowngrade}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T2 customer funds",
			decision: Decision{SchemaVersion: 1, Tier: TierT2, Rule: CodeCustomerFunds,
				Reasons: []string{CodeCustomerFunds}, ConfigHash: goldenConfigHash},
		},
		{
			name: "T0 cannot record high_value",
			decision: Decision{SchemaVersion: 1, Tier: TierT0, Rule: CodeHighValue,
				Reasons: []string{CodeHighValue}, ConfigHash: goldenConfigHash},
			wantErr: true,
		},
		{
			name: "T1 cannot record write_off_limit",
			decision: Decision{SchemaVersion: 1, Tier: TierT1, Rule: CodeWriteOffLimit,
				Reasons: []string{CodeWriteOffLimit}, ConfigHash: goldenConfigHash},
			wantErr: true,
		},
		{
			name: "T2 cannot record a gate rule",
			decision: Decision{SchemaVersion: 1, Tier: TierT2, Rule: CodeAutoApply,
				Reasons: []string{CodeAutoApply}, ConfigHash: goldenConfigHash},
			wantErr: true,
		},
		{
			name: "rule absent from reasons",
			decision: Decision{SchemaVersion: 1, Tier: TierT2, Rule: CodeHighValue,
				Reasons: []string{CodeCustomerFunds}, ConfigHash: goldenConfigHash},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decision.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidDecision) {
					t.Fatalf("Validate error = %v, want ErrInvalidDecision", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate error = %v, want nil", err)
			}
		})
	}
}

// TestDecisionGoldenJSON freezes the persisted decision shape and field order.
//
// covers: FIN-34
func TestDecisionGoldenJSON(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		golden   string
	}{
		{
			name: "t0",
			decision: Decision{
				SchemaVersion: 1,
				Tier:          TierT0,
				Rule:          CodeAutoApply,
				Reasons:       []string{CodeAutoApply},
				ConfigHash:    goldenConfigHash,
			},
			golden: "golden_decision_t0.json",
		},
		{
			name: "t1",
			decision: Decision{
				SchemaVersion: 1,
				Tier:          TierT1,
				Rule:          CodeVerifierMissing,
				Reasons:       []string{CodeVerifierMissing},
				ConfigHash:    goldenConfigHash,
			},
			golden: "golden_decision_t1.json",
		},
		{
			name: "t2",
			decision: Decision{
				SchemaVersion: 1,
				Tier:          TierT2,
				Rule:          CodeHighValue,
				Reasons:       []string{CodeHighValue, CodeCustomerFunds},
				ConfigHash:    goldenConfigHash,
			},
			golden: "golden_decision_t2.json",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.decision.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join("testdata", tc.golden))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			want := strings.TrimSpace(string(raw))
			if string(got) != want {
				t.Fatalf("decision JSON = %s, want golden %s", got, want)
			}
		})
	}
}
