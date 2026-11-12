package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/shopspring/decimal"
)

// ProposalType is one of the frozen proposal types.
type ProposalType string

// Proposal is the policy-relevant proposal snapshot. It contains no payload or
// rationale text, so the engine cannot be influenced by untrusted narrative.
type Proposal struct {
	ID              string
	CaseID          string
	Type            ProposalType
	Confidence      decimal.Decimal
	Evidence        []string
	PatternVerified *string
}

// Verification is the placeholder contract for the deterministic verifiers
// (M4-05). Details is opaque and never parsed here.
type Verification struct {
	Pattern string
	Passed  bool
	Details json.RawMessage
}

// Facts are caller-derived inputs the policy engine cannot query itself. A
// missing or wrong fact can only make a decision stricter, never looser.
type Facts struct {
	ValueMinor           int64
	Currency             string
	CaseCurrency         string
	LineCurrencies       []string
	AccountCodes         []string
	EvidenceValid        bool
	AutoApplyEnabled     bool
	RepeatedCounterparty bool
	Counterparty         string
}

// Options are engine-construction inputs that must not vary per call.
type Options struct {
	// CustomerFundsAccounts is the configured customer-funds account set
	// (FIN-13). Supplied once from the ledger config; a nil list disables the
	// predicate, and the ledger service remains the independent backstop.
	CustomerFundsAccounts []string
}

// Engine is an immutable evaluator: no DB, no clock, no network, no shared
// mutable state. It owns deep copies of every policy map/slice, so mutating the
// LoadedConfig or Options that built it can never change a decision. It is safe
// for concurrent use.
type Engine struct {
	config          Config
	hash            string
	customerFunds   map[string]struct{}
	allowedTypes    map[ProposalType]struct{}
	allowedPatterns map[string]struct{}
	downgradeBelow  decimal.Decimal
}

// NewEngine re-parses the exact bytes it is about to trust, validates that
// document, and requires LoadedConfig.Config to be deeply equal to that parse.
// A Config supplied independently of its YAML (even a valid one) is rejected
// with ErrConfigMismatch, so a decision's config hash always names the policy
// that produced it. All maps and slices are then deep-copied into the engine.
func NewEngine(loaded LoadedConfig, opts Options) (*Engine, error) {
	if !configHashPattern.MatchString(loaded.Hash) {
		return nil, fmt.Errorf("%w: config hash %q is not 64 lowercase hex characters", ErrInvalidConfig, loaded.Hash)
	}
	if got := HashYAML(loaded.YAML); got != loaded.Hash {
		return nil, fmt.Errorf("%w: config hash %q does not match sha256(%s)", ErrInvalidConfig, loaded.Hash, got)
	}
	parsed, err := parseConfig(loaded.YAML)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(parsed, loaded.Config) {
		return nil, fmt.Errorf("%w: config hash %s names a different policy document", ErrConfigMismatch, loaded.Hash)
	}

	config := cloneConfig(parsed)
	engine := &Engine{
		config:          config,
		hash:            loaded.Hash,
		customerFunds:   make(map[string]struct{}, len(opts.CustomerFundsAccounts)),
		allowedTypes:    make(map[ProposalType]struct{}, len(config.AutoApply.AllowedTypes)),
		allowedPatterns: make(map[string]struct{}, len(config.AutoApply.AllowedPatterns)),
		downgradeBelow:  config.Confidence.DowngradeBelow.Value(),
	}
	for _, account := range opts.CustomerFundsAccounts {
		engine.customerFunds[account] = struct{}{}
	}
	for _, proposalType := range config.AutoApply.AllowedTypes {
		engine.allowedTypes[ProposalType(proposalType)] = struct{}{}
	}
	for _, pattern := range config.AutoApply.AllowedPatterns {
		engine.allowedPatterns[pattern] = struct{}{}
	}
	return engine, nil
}

// Evaluate applies the fixed precedence from the policy contract: all T2
// predicates, then every T0 gate, then the confidence downgrade. It never
// consults a clock or database and never returns an error; unknown facts fail
// closed to a stricter tier with a machine reason.
func (e *Engine) Evaluate(p Proposal, v Verification, f Facts) Decision {
	if reasons := e.t2Reasons(p, f); len(reasons) > 0 {
		return e.decision(TierT2, reasons[0], reasons)
	}

	failed := e.t0GateFailures(p, v, f)
	if len(failed) == 0 {
		return e.applyConfidence(e.decision(TierT0, CodeAutoApply, []string{CodeAutoApply}), p, failed)
	}
	// The defensive default only applies if a T1 tier somehow has no failed
	// gate; with a valid, validated config it is unreachable.
	return e.applyConfidence(e.decision(TierT1, failed[0], failed), p, failed)
}

func (e *Engine) t2Reasons(p Proposal, f Facts) []string {
	reasons := make([]string, 0, 5)
	if f.Currency != "" {
		cap, ok := e.SingleApprovalCap(f.Currency)
		if !ok || f.ValueMinor > cap {
			reasons = append(reasons, CodeHighValue)
		}
	}
	if p.Type == TypeWriteOff && f.Currency != "" {
		cap, ok := e.WriteOffCap(f.Currency)
		if !ok || f.ValueMinor > cap {
			reasons = append(reasons, CodeWriteOffLimit)
		}
	}
	if crossCurrency(f) {
		reasons = append(reasons, CodeCrossCurrency)
	}
	if e.touchesCustomerFunds(f.AccountCodes) {
		reasons = append(reasons, CodeCustomerFunds)
	}
	if f.RepeatedCounterparty {
		reasons = append(reasons, CodeRepeatedCounterparty)
	}
	return reasons
}

// crossCurrency is the strengthened T2 predicate: every line currency must
// equal the case currency and the proposal value currency must equal it too.
// Exact string comparison is deliberate; unknown/empty values differ from a
// known currency and therefore fail closed to T2.
func crossCurrency(f Facts) bool {
	if f.Currency != f.CaseCurrency {
		return true
	}
	for _, currency := range f.LineCurrencies {
		if currency != f.CaseCurrency {
			return true
		}
	}
	return false
}

func (e *Engine) touchesCustomerFunds(accounts []string) bool {
	for _, account := range accounts {
		if _, ok := e.customerFunds[account]; ok {
			return true
		}
	}
	return false
}

func (e *Engine) t0GateFailures(p Proposal, v Verification, f Facts) []string {
	failed := make([]string, 0, 8)

	if _, ok := e.allowedTypes[p.Type]; !ok {
		failed = append(failed, CodeTypeNotAllowed)
	}
	if p.PatternVerified == nil {
		failed = append(failed, CodePatternNotAllowed)
	} else if _, ok := e.allowedPatterns[*p.PatternVerified]; !ok {
		failed = append(failed, CodePatternNotAllowed)
	}
	switch {
	case v.Pattern == "":
		failed = append(failed, CodeVerifierMissing)
	case !v.Passed:
		failed = append(failed, CodeVerifierFailed)
	}
	if p.PatternVerified != nil && v.Pattern != "" && *p.PatternVerified != v.Pattern {
		failed = append(failed, CodeVerifierMismatch)
	}
	if !f.EvidenceValid || len(p.Evidence) == 0 {
		failed = append(failed, CodeEvidenceInvalid)
	}
	if f.Currency == "" || f.ValueMinor < 0 {
		failed = append(failed, CodeValueUnknown)
	}
	if f.Currency != "" {
		if cap, ok := e.AutoApplyCap(f.Currency); !ok || f.ValueMinor > cap {
			failed = append(failed, CodeValueCap)
		}
	}
	if !f.AutoApplyEnabled {
		failed = append(failed, CodeAutoApplyDisabled)
	}
	return failed
}

func (e *Engine) applyConfidence(base Decision, p Proposal, failed []string) Decision {
	if !p.Confidence.LessThan(e.downgradeBelow) {
		return base
	}
	switch base.Tier {
	case TierT0:
		return e.decision(TierT1, CodeConfidenceDowngrade, []string{CodeAutoApply, CodeConfidenceDowngrade})
	case TierT1:
		reasons := append([]string(nil), failed...)
		reasons = append(reasons, CodeConfidenceDowngrade)
		return e.decision(TierT2, CodeConfidenceDowngrade, reasons)
	default:
		// T2 is already the strictest tier; confidence cannot change it.
		return base
	}
}

func (e *Engine) decision(tier Tier, rule string, reasons []string) Decision {
	return Decision{
		SchemaVersion: SchemaVersion,
		Tier:          tier,
		Rule:          rule,
		Reasons:       append([]string(nil), reasons...),
		ConfigHash:    e.hash,
	}
}

// AutoApplyCap returns the T0 cap for currency. Lookup is exact-case; a miss
// returns false and callers must fail closed.
func (e *Engine) AutoApplyCap(currency string) (int64, bool) {
	value, ok := e.config.AutoApply.MaxValueMinor[currency]
	return value, ok
}

// SingleApprovalCap returns the T2 high-value cap for currency.
func (e *Engine) SingleApprovalCap(currency string) (int64, bool) {
	value, ok := e.config.SingleApproval.MaxValueMinor[currency]
	return value, ok
}

// WriteOffCap returns the write-off limit for currency.
func (e *Engine) WriteOffCap(currency string) (int64, bool) {
	value, ok := e.config.WriteOff.MaxValueMinor[currency]
	return value, ok
}

// ProposalTTL returns the configured proposal lifetime for M4-11.
func (e *Engine) ProposalTTL() time.Duration { return e.config.ProposalTTL() }

// ConfigHash returns the content hash carried by every decision.
func (e *Engine) ConfigHash() string { return e.hash }
