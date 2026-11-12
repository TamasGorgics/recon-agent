package policy

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func dec(value string) decimal.Decimal { return decimal.RequireFromString(value) }

func strptr(value string) *string { return &value }

func testEngine(t *testing.T, customerFunds ...string) *Engine {
	t.Helper()
	loaded, err := LoadFile(committedPolicyFile)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	engine, err := NewEngine(loaded, Options{CustomerFundsAccounts: customerFunds})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func t0Proposal() Proposal {
	return Proposal{
		ID:              "case-1",
		CaseID:          "case-1",
		Type:            TypeLink,
		Confidence:      dec("0.95"),
		Evidence:        []string{"evidence-1"},
		PatternVerified: strptr(PatternFeeVariance),
	}
}

func t0Verification() Verification {
	return Verification{Pattern: PatternFeeVariance, Passed: true}
}

func t0Facts() Facts {
	return Facts{
		ValueMinor:       100,
		Currency:         "USD",
		CaseCurrency:     "USD",
		AccountCodes:     []string{"fees_expense"},
		EvidenceValid:    true,
		AutoApplyEnabled: true,
	}
}

type evaluateCase struct {
	name         string
	proposal     func(*Proposal)
	verification func(*Verification)
	facts        func(*Facts)
	wantTier     Tier
	wantRule     string
	wantReasons  []string
}

func runEvaluateCases(t *testing.T, engine *Engine, cases []evaluateCase) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := t0Proposal()
			v := t0Verification()
			f := t0Facts()
			if tc.proposal != nil {
				tc.proposal(&p)
			}
			if tc.verification != nil {
				tc.verification(&v)
			}
			if tc.facts != nil {
				tc.facts(&f)
			}
			got := engine.Evaluate(p, v, f)
			if got.Tier != tc.wantTier || got.Rule != tc.wantRule || !slices.Equal(got.Reasons, tc.wantReasons) {
				t.Fatalf("Evaluate = %s/%s/%v, want %s/%s/%v",
					got.Tier, got.Rule, got.Reasons, tc.wantTier, tc.wantRule, tc.wantReasons)
			}
			if got.SchemaVersion != 1 {
				t.Errorf("schema_version = %d, want 1", got.SchemaVersion)
			}
			if got.ConfigHash != engine.ConfigHash() {
				t.Errorf("config_hash = %q, want %q", got.ConfigHash, engine.ConfigHash())
			}
			if got.Reasons == nil {
				t.Error("reasons is nil")
			}
			if err := got.Validate(); err != nil {
				t.Errorf("decision validation: %v", err)
			}
		})
	}
}

// covers: FIN-13, FIN-34
func TestEvaluateT2Predicates(t *testing.T) {
	engine := testEngine(t, "customer_funds")
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name: "high value above single-approval cap",
			facts: func(f *Facts) {
				f.ValueMinor = 50001
			},
			wantTier: TierT2, wantRule: CodeHighValue, wantReasons: []string{CodeHighValue},
		},
		{
			name: "high value missing currency cap fails closed",
			facts: func(f *Facts) {
				f.ValueMinor = 1
				f.Currency = "KWD"
				f.CaseCurrency = "KWD"
			},
			wantTier: TierT2, wantRule: CodeHighValue, wantReasons: []string{CodeHighValue},
		},
		{
			name: "write-off above write-off cap",
			proposal: func(p *Proposal) {
				p.Type = TypeWriteOff
			},
			facts: func(f *Facts) {
				f.ValueMinor = 501
			},
			wantTier: TierT2, wantRule: CodeWriteOffLimit, wantReasons: []string{CodeWriteOffLimit},
		},
		{
			name: "two line currencies",
			facts: func(f *Facts) {
				f.LineCurrencies = []string{"USD", "EUR"}
			},
			wantTier: TierT2, wantRule: CodeCrossCurrency, wantReasons: []string{CodeCrossCurrency},
		},
		{
			name: "single line currency differs from case currency",
			facts: func(f *Facts) {
				f.LineCurrencies = []string{"EUR", "EUR"}
			},
			wantTier: TierT2, wantRule: CodeCrossCurrency, wantReasons: []string{CodeCrossCurrency},
		},
		{
			name: "empty line currency with known case currency fails closed",
			facts: func(f *Facts) {
				f.LineCurrencies = []string{""}
			},
			wantTier: TierT2, wantRule: CodeCrossCurrency, wantReasons: []string{CodeCrossCurrency},
		},
		{
			name: "missing value currency with known case currency fails closed",
			facts: func(f *Facts) {
				f.Currency = ""
			},
			wantTier: TierT2, wantRule: CodeCrossCurrency, wantReasons: []string{CodeCrossCurrency},
		},
		{
			name: "value currency differs from case currency",
			facts: func(f *Facts) {
				f.CaseCurrency = "EUR"
			},
			wantTier: TierT2, wantRule: CodeCrossCurrency, wantReasons: []string{CodeCrossCurrency},
		},
		{
			name: "customer funds account",
			facts: func(f *Facts) {
				f.AccountCodes = []string{"fees_expense", "customer_funds"}
			},
			wantTier: TierT2, wantRule: CodeCustomerFunds, wantReasons: []string{CodeCustomerFunds},
		},
		{
			name: "repeated counterparty",
			facts: func(f *Facts) {
				f.RepeatedCounterparty = true
			},
			wantTier: TierT2, wantRule: CodeRepeatedCounterparty, wantReasons: []string{CodeRepeatedCounterparty},
		},
	})
}

// covers: FIN-13, FIN-34
func TestEvaluateT2Dominates(t *testing.T) {
	engine := testEngine(t, "customer_funds")
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name: "customer funds with every T0 gate passing",
			facts: func(f *Facts) {
				f.AccountCodes = []string{"customer_funds"}
			},
			wantTier: TierT2, wantRule: CodeCustomerFunds, wantReasons: []string{CodeCustomerFunds},
		},
		{
			name: "high value with a passing verifier",
			facts: func(f *Facts) {
				f.ValueMinor = 50001
			},
			wantTier: TierT2, wantRule: CodeHighValue, wantReasons: []string{CodeHighValue},
		},
		{
			name: "all predicates in fixed order",
			facts: func(f *Facts) {
				f.ValueMinor = 50001
				f.LineCurrencies = []string{"USD", "EUR"}
				f.AccountCodes = []string{"customer_funds"}
				f.RepeatedCounterparty = true
			},
			wantTier: TierT2,
			wantRule: CodeHighValue,
			wantReasons: []string{
				CodeHighValue, CodeCrossCurrency, CodeCustomerFunds, CodeRepeatedCounterparty,
			},
		},
		{
			name: "high value precedes write-off limit",
			proposal: func(p *Proposal) {
				p.Type = TypeWriteOff
			},
			facts: func(f *Facts) {
				f.ValueMinor = 500001
			},
			wantTier: TierT2,
			wantRule: CodeHighValue,
			wantReasons: []string{
				CodeHighValue, CodeWriteOffLimit,
			},
		},
		{
			name: "confidence cannot loosen T2",
			proposal: func(p *Proposal) {
				p.Confidence = decimal.Zero
			},
			facts: func(f *Facts) {
				f.ValueMinor = 50001
			},
			wantTier: TierT2, wantRule: CodeHighValue, wantReasons: []string{CodeHighValue},
		},
	})
}

// covers: FIN-13, FIN-34
func TestEvaluatePerCurrencyBoundaries(t *testing.T) {
	engine := testEngine(t)
	type currencyCaps struct {
		currency string
		auto     int64
		single   int64
		writeOff int64
	}
	for _, caps := range []currencyCaps{
		{"USD", 500, 50000, 500},
		{"EUR", 500, 50000, 500},
		{"SEK", 5000, 500000, 5000},
		{"JPY", 500, 50000, 500},
	} {
		caps := caps
		t.Run(caps.currency, func(t *testing.T) {
			t.Run("auto_cap_is_T0", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.auto
				wantDecision(t, engine, p, v, f, TierT0, CodeAutoApply, []string{CodeAutoApply})
			})
			t.Run("auto_cap_plus_one_is_T1_value_cap", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.auto+1
				wantDecision(t, engine, p, v, f, TierT1, CodeValueCap, []string{CodeValueCap})
			})
			t.Run("single_approval_cap_is_below_T2", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.single
				wantDecision(t, engine, p, v, f, TierT1, CodeValueCap, []string{CodeValueCap})
			})
			t.Run("single_approval_cap_plus_one_is_T2", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.single+1
				wantDecision(t, engine, p, v, f, TierT2, CodeHighValue, []string{CodeHighValue})
			})
			t.Run("write_off_cap_is_T1_type_not_allowed", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				p.Type = TypeWriteOff
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.writeOff
				wantDecision(t, engine, p, v, f, TierT1, CodeTypeNotAllowed, []string{CodeTypeNotAllowed})
			})
			t.Run("write_off_cap_plus_one_is_T2", func(t *testing.T) {
				p, v, f := t0Proposal(), t0Verification(), t0Facts()
				p.Type = TypeWriteOff
				f.Currency, f.CaseCurrency, f.ValueMinor = caps.currency, caps.currency, caps.writeOff+1
				wantDecision(t, engine, p, v, f, TierT2, CodeWriteOffLimit, []string{CodeWriteOffLimit})
			})
		})
	}

	t.Run("write_off_missing_currency_cap_fails_closed", func(t *testing.T) {
		p, v, f := t0Proposal(), t0Verification(), t0Facts()
		p.Type = TypeWriteOff
		f.Currency, f.CaseCurrency, f.ValueMinor = "KWD", "KWD", 1
		wantDecision(t, engine, p, v, f, TierT2, CodeHighValue, []string{CodeHighValue, CodeWriteOffLimit})
	})
}

func wantDecision(t *testing.T, engine *Engine, p Proposal, v Verification, f Facts, tier Tier, rule string, reasons []string) {
	t.Helper()
	got := engine.Evaluate(p, v, f)
	if got.Tier != tier || got.Rule != rule || !slices.Equal(got.Reasons, reasons) {
		t.Fatalf("Evaluate = %s/%s/%v, want %s/%s/%v", got.Tier, got.Rule, got.Reasons, tier, rule, reasons)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("decision validation: %v", err)
	}
}

// covers: FIN-13, FIN-34
func TestEvaluateT0Gates(t *testing.T) {
	engine := testEngine(t, "customer_funds")
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name: "disallowed type write_off",
			proposal: func(p *Proposal) {
				p.Type = TypeWriteOff
			},
			wantTier: TierT1, wantRule: CodeTypeNotAllowed, wantReasons: []string{CodeTypeNotAllowed},
		},
		{
			name: "disallowed type duplicate",
			proposal: func(p *Proposal) {
				p.Type = TypeDuplicate
			},
			wantTier: TierT1, wantRule: CodeTypeNotAllowed, wantReasons: []string{CodeTypeNotAllowed},
		},
		{
			name: "disallowed type request_info",
			proposal: func(p *Proposal) {
				p.Type = TypeRequestInfo
			},
			wantTier: TierT1, wantRule: CodeTypeNotAllowed, wantReasons: []string{CodeTypeNotAllowed},
		},
		{
			name: "other pattern not allowed",
			proposal: func(p *Proposal) {
				p.PatternVerified = strptr(PatternOther)
			},
			verification: func(v *Verification) {
				v.Pattern = PatternOther
			},
			wantTier: TierT1, wantRule: CodePatternNotAllowed, wantReasons: []string{CodePatternNotAllowed},
		},
		{
			name: "nil pattern_verified",
			proposal: func(p *Proposal) {
				p.PatternVerified = nil
			},
			wantTier: TierT1, wantRule: CodePatternNotAllowed, wantReasons: []string{CodePatternNotAllowed},
		},
		{
			name: "missing verifier",
			verification: func(v *Verification) {
				*v = Verification{}
			},
			wantTier: TierT1, wantRule: CodeVerifierMissing, wantReasons: []string{CodeVerifierMissing},
		},
		{
			name: "failed verifier",
			verification: func(v *Verification) {
				v.Passed = false
			},
			wantTier: TierT1, wantRule: CodeVerifierFailed, wantReasons: []string{CodeVerifierFailed},
		},
		{
			name: "verifier pattern mismatch",
			proposal: func(p *Proposal) {
				p.PatternVerified = strptr(PatternRounding)
			},
			wantTier: TierT1, wantRule: CodeVerifierMismatch, wantReasons: []string{CodeVerifierMismatch},
		},
		{
			name: "evidence invalid",
			facts: func(f *Facts) {
				f.EvidenceValid = false
			},
			wantTier: TierT1, wantRule: CodeEvidenceInvalid, wantReasons: []string{CodeEvidenceInvalid},
		},
		{
			name: "empty evidence list",
			proposal: func(p *Proposal) {
				p.Evidence = nil
			},
			wantTier: TierT1, wantRule: CodeEvidenceInvalid, wantReasons: []string{CodeEvidenceInvalid},
		},
		{
			name: "value above auto-apply cap",
			facts: func(f *Facts) {
				f.ValueMinor = 501
			},
			wantTier: TierT1, wantRule: CodeValueCap, wantReasons: []string{CodeValueCap},
		},
		{
			name: "empty currency",
			facts: func(f *Facts) {
				f.Currency = ""
				f.CaseCurrency = ""
			},
			wantTier: TierT1, wantRule: CodeValueUnknown, wantReasons: []string{CodeValueUnknown},
		},
		{
			name: "negative value",
			facts: func(f *Facts) {
				f.ValueMinor = -1
			},
			wantTier: TierT1, wantRule: CodeValueUnknown, wantReasons: []string{CodeValueUnknown},
		},
		{
			name: "auto apply disabled",
			facts: func(f *Facts) {
				f.AutoApplyEnabled = false
			},
			wantTier: TierT1, wantRule: CodeAutoApplyDisabled, wantReasons: []string{CodeAutoApplyDisabled},
		},
		{
			name: "all gates fail in fixed order",
			proposal: func(p *Proposal) {
				p.Type = TypeWriteOff
				p.PatternVerified = nil
				p.Evidence = nil
			},
			verification: func(v *Verification) {
				*v = Verification{}
			},
			facts: func(f *Facts) {
				f.Currency = ""
				f.CaseCurrency = ""
				f.ValueMinor = -1
				f.EvidenceValid = false
				f.AutoApplyEnabled = false
			},
			wantTier: TierT1,
			wantRule: CodeTypeNotAllowed,
			wantReasons: []string{
				CodeTypeNotAllowed,
				CodePatternNotAllowed,
				CodeVerifierMissing,
				CodeEvidenceInvalid,
				CodeValueUnknown,
				CodeAutoApplyDisabled,
			},
		},
	})
}

// covers: FIN-13, FIN-34
func TestEvaluateT0Reachable(t *testing.T) {
	engine := testEngine(t)
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name:     "full gate set",
			wantTier: TierT0, wantRule: CodeAutoApply, wantReasons: []string{CodeAutoApply},
		},
		{
			name: "zero value link is T0",
			facts: func(f *Facts) {
				f.ValueMinor = 0
			},
			wantTier: TierT0, wantRule: CodeAutoApply, wantReasons: []string{CodeAutoApply},
		},
		{
			name: "adjustment with line and case currencies matching",
			proposal: func(p *Proposal) {
				p.Type = TypeAdjustment
			},
			facts: func(f *Facts) {
				f.LineCurrencies = []string{"USD", "USD"}
			},
			wantTier: TierT0, wantRule: CodeAutoApply, wantReasons: []string{CodeAutoApply},
		},
	})
}

// covers: FIN-13, FIN-34
func TestEvaluateCustomerFundsOptions(t *testing.T) {
	p, v, f := t0Proposal(), t0Verification(), t0Facts()
	f.AccountCodes = []string{"customer_funds"}

	withFunds := testEngine(t, "customer_funds")
	if got := withFunds.Evaluate(p, v, f); got.Tier != TierT2 || got.Rule != CodeCustomerFunds {
		t.Fatalf("customer funds with configured list = %s/%s, want T2/customer_funds", got.Tier, got.Rule)
	}
	withoutFunds := testEngine(t)
	if got := withoutFunds.Evaluate(p, v, f); got.Tier != TierT0 || got.Rule != CodeAutoApply {
		t.Fatalf("customer funds with nil list = %s/%s, want T0/auto_apply", got.Tier, got.Rule)
	}
}

// covers: FIN-13, FIN-34
func TestEvaluateT1Defaults(t *testing.T) {
	engine := testEngine(t)
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name: "no verifier",
			verification: func(v *Verification) {
				*v = Verification{}
			},
			wantTier: TierT1, wantRule: CodeVerifierMissing, wantReasons: []string{CodeVerifierMissing},
		},
		{
			name: "failed verifier",
			verification: func(v *Verification) {
				v.Passed = false
			},
			wantTier: TierT1, wantRule: CodeVerifierFailed, wantReasons: []string{CodeVerifierFailed},
		},
		{
			name: "new pattern",
			proposal: func(p *Proposal) {
				p.PatternVerified = strptr(PatternOther)
			},
			verification: func(v *Verification) {
				v.Pattern = PatternOther
			},
			wantTier: TierT1, wantRule: CodePatternNotAllowed, wantReasons: []string{CodePatternNotAllowed},
		},
		{
			name: "disallowed type with passing verifier",
			proposal: func(p *Proposal) {
				p.Type = TypeDuplicate
			},
			wantTier: TierT1, wantRule: CodeTypeNotAllowed, wantReasons: []string{CodeTypeNotAllowed},
		},
	})
}

// covers: FIN-13, FIN-34
func TestEvaluateConfidence(t *testing.T) {
	engine := testEngine(t)
	runEvaluateCases(t, engine, []evaluateCase{
		{
			name: "0.99 cannot upgrade T1",
			proposal: func(p *Proposal) {
				p.Confidence = dec("0.99")
			},
			verification: func(v *Verification) {
				*v = Verification{}
			},
			wantTier: TierT1, wantRule: CodeVerifierMissing, wantReasons: []string{CodeVerifierMissing},
		},
		{
			name: "0.8 exactly does not downgrade T0",
			proposal: func(p *Proposal) {
				p.Confidence = dec("0.8")
			},
			wantTier: TierT0, wantRule: CodeAutoApply, wantReasons: []string{CodeAutoApply},
		},
		{
			name: "0.8 exactly does not downgrade T1",
			proposal: func(p *Proposal) {
				p.Confidence = dec("0.8")
			},
			verification: func(v *Verification) {
				*v = Verification{}
			},
			wantTier: TierT1, wantRule: CodeVerifierMissing, wantReasons: []string{CodeVerifierMissing},
		},
		{
			name: "0.7999 downgrades T0 to T1",
			proposal: func(p *Proposal) {
				p.Confidence = dec("0.7999")
			},
			wantTier:    TierT1,
			wantRule:    CodeConfidenceDowngrade,
			wantReasons: []string{CodeAutoApply, CodeConfidenceDowngrade},
		},
		{
			name: "zero confidence downgrades T0 to T1",
			proposal: func(p *Proposal) {
				p.Confidence = decimal.Zero
			},
			wantTier:    TierT1,
			wantRule:    CodeConfidenceDowngrade,
			wantReasons: []string{CodeAutoApply, CodeConfidenceDowngrade},
		},
		{
			name: "0.7999 downgrades T1 to T2",
			proposal: func(p *Proposal) {
				p.Confidence = dec("0.7999")
			},
			verification: func(v *Verification) {
				*v = Verification{}
			},
			wantTier:    TierT2,
			wantRule:    CodeConfidenceDowngrade,
			wantReasons: []string{CodeVerifierMissing, CodeConfidenceDowngrade},
		},
		{
			name: "zero confidence downgrades failed verifier T1 to T2",
			proposal: func(p *Proposal) {
				p.Confidence = decimal.Zero
			},
			verification: func(v *Verification) {
				v.Passed = false
			},
			wantTier:    TierT2,
			wantRule:    CodeConfidenceDowngrade,
			wantReasons: []string{CodeVerifierFailed, CodeConfidenceDowngrade},
		},
		{
			name: "multiple gate reasons precede confidence",
			proposal: func(p *Proposal) {
				p.Type = TypeWriteOff
				p.Confidence = dec("0.7999")
			},
			facts: func(f *Facts) {
				f.EvidenceValid = false
			},
			wantTier:    TierT2,
			wantRule:    CodeConfidenceDowngrade,
			wantReasons: []string{CodeTypeNotAllowed, CodeEvidenceInvalid, CodeConfidenceDowngrade},
		},
		{
			name: "T2 stays T2 with low confidence",
			proposal: func(p *Proposal) {
				p.Confidence = decimal.Zero
			},
			facts: func(f *Facts) {
				f.ValueMinor = 50001
			},
			wantTier: TierT2, wantRule: CodeHighValue, wantReasons: []string{CodeHighValue},
		},
	})
}

// covers: FIN-34
func TestEvaluateDeterministic(t *testing.T) {
	engine := testEngine(t)
	p, v, f := t0Proposal(), t0Verification(), t0Facts()
	p.Type = TypeWriteOff
	p.PatternVerified = nil
	p.Confidence = dec("0.5")
	f.EvidenceValid = false
	f.ValueMinor = -1
	f.AutoApplyEnabled = false

	first := engine.Evaluate(p, v, f)
	firstJSON, err := first.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	for i := 0; i < 20; i++ {
		next := engine.Evaluate(p, v, f)
		nextJSON, err := next.JSON()
		if err != nil {
			t.Fatalf("JSON: %v", err)
		}
		if string(nextJSON) != string(firstJSON) {
			t.Fatalf("decision JSON changed: %s vs %s", nextJSON, firstJSON)
		}
	}
	if !strings.Contains(string(firstJSON), `"tier":"T2"`) {
		t.Fatalf("decision JSON = %s, want T2", firstJSON)
	}
}

// TestEngineConfigFrozenAfterConstruction proves the engine owns deep copies:
// post-construction mutation of the caller's LoadedConfig/Options cannot change
// a decision or an accessor.
//
// covers: FIN-34
func TestEngineConfigFrozenAfterConstruction(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	opts := Options{CustomerFundsAccounts: []string{"customer_funds"}}
	engine, err := NewEngine(loaded, opts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	p, v, f := t0Proposal(), t0Verification(), t0Facts()
	f.ValueMinor = 501
	before := engine.Evaluate(p, v, f)
	if before.Tier != TierT1 || before.Rule != CodeValueCap {
		t.Fatalf("before mutation = %s/%s, want T1/value_cap", before.Tier, before.Rule)
	}

	loaded.Config.AutoApply.MaxValueMinor["USD"] = 1_000_000
	loaded.Config.SingleApproval.MaxValueMinor["USD"] = 1
	loaded.Config.WriteOff.MaxValueMinor["USD"] = 1
	loaded.Config.AutoApply.AllowedTypes[0] = string(TypeWriteOff)
	loaded.Config.AutoApply.AllowedPatterns[0] = PatternOther
	loaded.Config.ProposalTTLHours = 1
	opts.CustomerFundsAccounts[0] = "not_customer_funds"

	after := engine.Evaluate(p, v, f)
	if after.Tier != before.Tier || after.Rule != before.Rule || !slices.Equal(after.Reasons, before.Reasons) {
		t.Fatalf("caller mutation changed the decision: %s/%s/%v -> %s/%s/%v",
			before.Tier, before.Rule, before.Reasons, after.Tier, after.Rule, after.Reasons)
	}
	if cap, ok := engine.AutoApplyCap("USD"); !ok || cap != 500 {
		t.Fatalf("AutoApplyCap(USD) after mutation = %d/%t, want 500/true", cap, ok)
	}
	if cap, ok := engine.SingleApprovalCap("USD"); !ok || cap != 50000 {
		t.Fatalf("SingleApprovalCap(USD) after mutation = %d/%t, want 50000/true", cap, ok)
	}
	if cap, ok := engine.WriteOffCap("USD"); !ok || cap != 500 {
		t.Fatalf("WriteOffCap(USD) after mutation = %d/%t, want 500/true", cap, ok)
	}
	if got := engine.ProposalTTL(); got != 48*time.Hour {
		t.Fatalf("ProposalTTL after mutation = %s, want 48h", got)
	}

	fundFacts := t0Facts()
	fundFacts.AccountCodes = []string{"customer_funds"}
	if got := engine.Evaluate(t0Proposal(), t0Verification(), fundFacts); got.Tier != TierT2 || got.Rule != CodeCustomerFunds {
		t.Fatalf("customer-funds options after mutation = %s/%s, want T2/customer_funds", got.Tier, got.Rule)
	}
}

// TestEngineConcurrentCallerMutation is the -race companion to the freeze test:
// Evaluate runs concurrently with caller-side mutation of the maps/slices that
// built the engine.
//
// covers: FIN-34
func TestEngineConcurrentCallerMutation(t *testing.T) {
	loaded, err := LoadBytes([]byte(validPolicyDoc))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	engine, err := NewEngine(loaded, Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	want := engine.Evaluate(t0Proposal(), t0Verification(), t0Facts())

	stop := make(chan struct{})
	var mutator sync.WaitGroup
	mutator.Add(1)
	go func() {
		defer mutator.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			loaded.Config.AutoApply.MaxValueMinor["USD"] = 1
			loaded.Config.SingleApproval.MaxValueMinor["USD"] = 1
			loaded.Config.WriteOff.MaxValueMinor["USD"] = 1
			loaded.Config.AutoApply.AllowedTypes[0] = string(TypeWriteOff)
			loaded.Config.AutoApply.AllowedPatterns[0] = PatternOther
			loaded.Config.ProposalTTLHours = 1
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan string, 1)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				got := engine.Evaluate(t0Proposal(), t0Verification(), t0Facts())
				if got.Tier != want.Tier || got.Rule != want.Rule || !slices.Equal(got.Reasons, want.Reasons) {
					select {
					case errCh <- "decision changed under caller mutation":
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	mutator.Wait()
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
}

// covers: FIN-13, FIN-34
func TestEvaluateConcurrent(t *testing.T) {
	engine := testEngine(t, "customer_funds")
	want := engine.Evaluate(t0Proposal(), t0Verification(), t0Facts())
	wantJSON, err := want.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan string, 1)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				got := engine.Evaluate(t0Proposal(), t0Verification(), t0Facts())
				data, err := got.JSON()
				if err != nil || string(data) != string(wantJSON) {
					select {
					case errCh <- "concurrent evaluation differs":
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case msg := <-errCh:
		t.Fatal(msg)
	default:
	}
}
