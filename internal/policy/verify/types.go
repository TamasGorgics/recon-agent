package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/money"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

// SchemaVersion is the only supported Details schema version.
const SchemaVersion = 1

// Pattern names. They mirror the frozen policy and proposals vocabularies; the
// verifier does not import policy pattern constants so its only policy
// dependency is policy.Verification.
const (
	PatternFeeVariance   = "fee_variance"
	PatternFXRounding    = "fx_rounding"
	PatternRounding      = "rounding"
	PatternReferenceTypo = "reference_typo"
	PatternOther         = "other"
)

// Verifier tuning defaults and bounds.
const (
	// DefaultRoundingCapMinor is the cap used for a currency absent from
	// Options.RoundingCapMinor.
	DefaultRoundingCapMinor int64 = 1
	// DefaultReferenceWindowDays is the inclusive absolute date window used
	// when Options.ReferenceWindowDays is zero.
	DefaultReferenceWindowDays = 3
	// MinReferenceWindowDays and MaxReferenceWindowDays bound the window after
	// defaults are applied.
	MinReferenceWindowDays = 1
	MaxReferenceWindowDays = 365
)

// Closed machine reason codes. Every failed Result uses one or more of these;
// the vocabulary is deliberately stable and contains no untrusted text.
const (
	ReasonNoPattern          = "no_pattern"
	ReasonPatternMismatch    = "pattern_mismatch"
	ReasonUnknownPattern     = "unknown_pattern"
	ReasonSourceAmbiguous    = "source_ambiguous"
	ReasonCurrencyMismatch   = "currency_mismatch"
	ReasonAmountOverflow     = "amount_overflow"
	ReasonInvalidInput       = "invalid_input"
	ReasonInvalidOptions     = "invalid_options"
	ReasonInvalidRate        = "invalid_rate"
	ReasonMissingSchedule    = "missing_schedule"
	ReasonAmbiguousSchedule  = "ambiguous_schedule"
	ReasonFeeMismatch        = "fee_mismatch"
	ReasonMissingRate        = "missing_rate"
	ReasonAmbiguousRate      = "ambiguous_rate"
	ReasonConversionMismatch = "conversion_mismatch"
	ReasonCapExceeded        = "cap_exceeded"
	ReasonNotRounding        = "not_rounding"
	ReasonAdjustmentMismatch = "adjustment_mismatch"
	ReasonNoCandidate        = "no_candidate"
	ReasonMultipleCandidates = "multiple_candidates"
	ReasonCandidateMismatch  = "candidate_mismatch"
	ReasonBreakUnresolved    = "break_unresolved"
)

var knownPatterns = map[string]struct{}{
	PatternFeeVariance:   {},
	PatternFXRounding:    {},
	PatternRounding:      {},
	PatternReferenceTypo: {},
	PatternOther:         {},
}

var reasonCodes = map[string]struct{}{
	ReasonNoPattern:          {},
	ReasonPatternMismatch:    {},
	ReasonUnknownPattern:     {},
	ReasonSourceAmbiguous:    {},
	ReasonCurrencyMismatch:   {},
	ReasonAmountOverflow:     {},
	ReasonInvalidInput:       {},
	ReasonInvalidOptions:     {},
	ReasonInvalidRate:        {},
	ReasonMissingSchedule:    {},
	ReasonAmbiguousSchedule:  {},
	ReasonFeeMismatch:        {},
	ReasonMissingRate:        {},
	ReasonAmbiguousRate:      {},
	ReasonConversionMismatch: {},
	ReasonCapExceeded:        {},
	ReasonNotRounding:        {},
	ReasonAdjustmentMismatch: {},
	ReasonNoCandidate:        {},
	ReasonMultipleCandidates: {},
	ReasonCandidateMismatch:  {},
	ReasonBreakUnresolved:    {},
}

// ErrInvalidOptions reports verifier options that cannot be used. Callers
// should validate options once when constructing a service; pure evaluators
// also fail closed with ReasonInvalidOptions instead of panicking.
var ErrInvalidOptions = errors.New("verify: invalid options")

var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Result is the deterministic verifier outcome. Passed is true only when the
// claimed pattern is confirmed by source data. Reasons are sorted, closed
// machine codes; the first code is the primary reason.
type Result struct {
	Pattern string
	Passed  bool
	Details Details
}

// Details is the versioned, machine-readable verifier document stored in
// proposals.verification. Field order and JSON names are frozen.
type Details struct {
	SchemaVersion int      `json:"schema_version"`
	Claimed       string   `json:"claimed,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
	ExpectedMinor int64    `json:"expected_minor,omitempty"`
	ActualMinor   int64    `json:"actual_minor,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	ScheduleID    string   `json:"schedule_id,omitempty"`
	Base          string   `json:"base,omitempty"`
	Quote         string   `json:"quote,omitempty"`
	AsOf          string   `json:"as_of,omitempty"`
	Candidates    int      `json:"candidates,omitempty"`
}

// Verification converts the result into the frozen policy hand-off shape. The
// details are always schema-version 1 evaluator output; callers must not
// interpret them as trusted narrative.
func (r Result) Verification() (policy.Verification, error) {
	details := r.Details
	if details.SchemaVersion == 0 {
		details.SchemaVersion = SchemaVersion
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return policy.Verification{}, fmt.Errorf("verify: marshal details: %w", err)
	}
	return policy.Verification{
		Pattern: r.Pattern,
		Passed:  r.Passed,
		Details: encoded,
	}, nil
}

// Options are verifier-construction inputs. A currency absent from
// RoundingCapMinor (including when the map is nil or empty) uses
// DefaultRoundingCapMinor; a zero ReferenceWindowDays uses
// DefaultReferenceWindowDays. The zero Options value is therefore valid and
// equivalent to DefaultOptions.
type Options struct {
	RoundingCapMinor    map[string]int64
	ReferenceWindowDays int
}

// DefaultOptions returns the built-in verifier defaults.
func DefaultOptions() Options {
	return Options{
		RoundingCapMinor:    nil,
		ReferenceWindowDays: DefaultReferenceWindowDays,
	}
}

// Validate rejects negative caps, malformed currency keys, and a reference
// window outside 1..365 after zero/blank defaults are applied.
func (o Options) Validate() error {
	o = o.withDefaults()
	for currency, cap := range o.RoundingCapMinor {
		if !currencyCodePattern.MatchString(currency) {
			return fmt.Errorf("%w: rounding cap currency %q is not an ISO 4217 code", ErrInvalidOptions, currency)
		}
		if cap < 0 {
			return fmt.Errorf("%w: rounding cap for %s is negative: %d", ErrInvalidOptions, currency, cap)
		}
	}
	if o.ReferenceWindowDays < MinReferenceWindowDays || o.ReferenceWindowDays > MaxReferenceWindowDays {
		return fmt.Errorf("%w: reference window days must be between %d and %d, got %d",
			ErrInvalidOptions, MinReferenceWindowDays, MaxReferenceWindowDays, o.ReferenceWindowDays)
	}
	return nil
}

// OptionsFromConfig maps the root config verifier block into verifier options.
// The cap map is copied so later mutation of the Config cannot change an
// Options value held by a service.
func OptionsFromConfig(c config.VerifierConfig) Options {
	c = c.WithDefaults()
	var caps map[string]int64
	if c.RoundingCapMinor != nil {
		caps = make(map[string]int64, len(c.RoundingCapMinor))
		for currency, cap := range c.RoundingCapMinor {
			caps[currency] = cap
		}
	}
	return Options{RoundingCapMinor: caps, ReferenceWindowDays: c.ReferenceWindowDays}
}

// withDefaults resolves a zero window without mutating the receiver's map.
func (o Options) withDefaults() Options {
	if o.ReferenceWindowDays == 0 {
		o.ReferenceWindowDays = DefaultReferenceWindowDays
	}
	return o
}

// capFor returns the configured rounding cap for currency, falling back to
// DefaultRoundingCapMinor. A nil or empty map therefore tolerates one minor
// unit by default.
func (o Options) capFor(currency string) int64 {
	if cap, ok := o.RoundingCapMinor[currency]; ok {
		return cap
	}
	return DefaultRoundingCapMinor
}

// ExternalRecord is the minimal projection of one external settlement record.
type ExternalRecord struct {
	Source, SourceRecordID, Kind, Currency string
	AmountMinor, FeeMinor                  int64 // amount_minor is net; gross = AmountMinor + FeeMinor
	OccurredAt                             time.Time
	// ValueDate is the provider's business date. reference_typo anchors its
	// inclusive window on it; when zero the evaluator falls back to
	// OccurredAt. Fee schedule selection continues to use OccurredAt.
	ValueDate time.Time
}

// LedgerTxn is the minimal projection of one cited ledger transaction.
type LedgerTxn struct {
	ID        string
	ValueDate time.Time
	Currency  string
	FeeMinor  int64 // sum of debit entries on fees_expense_*
	NetMinor  int64 // debits minus credits (signed net) on the scope ledger account
}

// AdjustmentLine is one balanced proposal line.
type AdjustmentLine struct {
	AccountCode string
	Direction   string // debit|credit
	AmountMinor int64
	Currency    string
}

// FeeSchedule is one fee_schedules row. ValidTo zero means open-ended.
type FeeSchedule struct {
	ID, Source, Kind, Currency string
	PctBPS                     int
	FixedMinor                 int64
	ValidFrom, ValidTo         time.Time
}

// FXRate is one fx_rates row. Rate is the exact decimal text, at most 12dp.
type FXRate struct {
	Base, Quote, Source string
	Rate                string
	AsOf                time.Time
}

// Candidate is one eligible ledger transaction for reference_typo.
type Candidate struct {
	LedgerTxnID string
	AmountMinor int64
	Currency    string
	ValueDate   time.Time
}

// FeeVarianceInput is the fee_variance pure evaluator input.
type FeeVarianceInput struct {
	Claimed   string
	External  ExternalRecord
	Ledger    LedgerTxn
	Schedules []FeeSchedule
	Lines     []AdjustmentLine
}

// FXRoundingInput is the fx_rounding pure evaluator input. Rates must be the
// service-scoped rows for the relevant base/quote pair and source.
type FXRoundingInput struct {
	Claimed  string
	External ExternalRecord
	Ledger   LedgerTxn
	Rates    []FXRate
	Lines    []AdjustmentLine
}

// RoundingInput is the rounding pure evaluator input.
type RoundingInput struct {
	Claimed  string
	External ExternalRecord
	Ledger   LedgerTxn
	Lines    []AdjustmentLine
}

// ReferenceTypoInput is the reference_typo pure evaluator input. Candidates is
// the service-scoped eligible pool; ProposedLedgerIDs is the normalized ledger
// set cited by the proposal payload.
type ReferenceTypoInput struct {
	Claimed           string
	External          ExternalRecord
	Candidates        []Candidate
	ProposedLedgerIDs []string
}

// ReasonCodes returns the closed reason vocabulary in sorted order.
func ReasonCodes() []string {
	codes := make([]string, 0, len(reasonCodes))
	for code := range reasonCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// IsReasonCode reports whether code belongs to the closed vocabulary.
func IsReasonCode(code string) bool {
	_, ok := reasonCodes[code]
	return ok
}

// failure builds a failed result with a schema-versioned, sorted, de-duplicated
// reason list. The first sorted reason is also the primary Reason.
func failure(pattern, claimed string, details Details, reasons ...string) Result {
	details.SchemaVersion = SchemaVersion
	details.Claimed = claimed
	details.Reasons = normalizeReasons(reasons)
	details.Reason = ""
	if len(details.Reasons) > 0 {
		details.Reason = details.Reasons[0]
	}
	return Result{Pattern: pattern, Passed: false, Details: details}
}

// success builds a passing result. A pass never carries reason codes.
func success(pattern, claimed string, details Details) Result {
	details.SchemaVersion = SchemaVersion
	details.Claimed = claimed
	details.Reasons = nil
	details.Reason = ""
	return Result{Pattern: pattern, Passed: true, Details: details}
}

// normalizeReasons drops empty strings, sorts, and de-duplicates the codes.
func normalizeReasons(reasons []string) []string {
	if len(reasons) == 0 {
		return nil
	}
	unique := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		if reason == "" {
			continue
		}
		unique[reason] = struct{}{}
	}
	if len(unique) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(unique))
	for reason := range unique {
		normalized = append(normalized, reason)
	}
	sort.Strings(normalized)
	return normalized
}

// checkClaim verifies the untrusted claim against the dispatched pattern. It
// returns (zero Result, true) when evaluation may continue.
func checkClaim(claimed, pattern string, details Details) (Result, bool) {
	if claimed == "" {
		return failure(pattern, claimed, details, ReasonNoPattern), false
	}
	if _, ok := knownPatterns[claimed]; !ok {
		return failure(pattern, claimed, details, ReasonUnknownPattern), false
	}
	if claimed != pattern {
		return failure(pattern, claimed, details, ReasonPatternMismatch), false
	}
	return Result{}, true
}

// resolveOptions validates options and applies their zero-value defaults.
func resolveOptions(opts Options) (Options, error) {
	opts = opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return Options{}, err
	}
	return opts, nil
}

// validCurrency reports whether code is a supported ISO 4217 code.
func validCurrency(code string) bool {
	if !currencyCodePattern.MatchString(code) {
		return false
	}
	// money.ParseCurrency is the single source of truth for exponents.
	_, err := money.ParseCurrency(code)
	return err == nil
}

// addInt64 returns a+b and whether the result fit in int64.
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// subInt64 returns a-b and whether the result fit in int64.
func subInt64(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

// absInt64 returns |v| and whether it fit; math.MinInt64 has no positive
// int64 representation.
func absInt64(v int64) (int64, bool) {
	if v == math.MinInt64 {
		return 0, false
	}
	if v < 0 {
		return -v, true
	}
	return v, true
}

// utcDate truncates t to its UTC calendar date.
func utcDate(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// referenceDate is the anchor for reference_typo's inclusive date window: the
// external business value date when present, otherwise the occurrence instant.
// Fee schedule selection deliberately keeps using OccurredAt.
func referenceDate(external ExternalRecord) time.Time {
	if !external.ValueDate.IsZero() {
		return external.ValueDate
	}
	return external.OccurredAt
}

// daysBetween returns the absolute number of calendar days between two dates.
// The time package's Duration cannot represent the full time.Time range, so
// this uses UTC midnight Unix days, which are exact.
func daysBetween(a, b time.Time) int64 {
	dayA := utcDate(a).Unix() / 86_400
	dayB := utcDate(b).Unix() / 86_400
	if dayA > dayB {
		return dayA - dayB
	}
	return dayB - dayA
}

// withinDateWindow reports whether candidate is within window inclusive days
// of target.
func withinDateWindow(target, candidate time.Time, window int) bool {
	return daysBetween(target, candidate) <= int64(window)
}

// validAdjustment checks the balanced adjustment rules in sourceCurrency for
// expected. It returns every violated reason code, not just the first.
func validAdjustment(lines []AdjustmentLine, sourceCurrency string, expected int64) []string {
	if expected <= 0 {
		return []string{ReasonAdjustmentMismatch}
	}
	if len(lines) == 0 {
		return []string{ReasonAdjustmentMismatch}
	}
	var reasons []string
	var debitTotal, creditTotal int64
	debitSeen, creditSeen := false, false
	for _, line := range lines {
		if line.Currency != sourceCurrency || line.AmountMinor < 0 {
			reasons = append(reasons, ReasonAdjustmentMismatch)
		}
		switch line.Direction {
		case "debit":
			debitSeen = true
			if next, ok := addInt64(debitTotal, line.AmountMinor); ok {
				debitTotal = next
			} else {
				reasons = append(reasons, ReasonAmountOverflow)
			}
		case "credit":
			creditSeen = true
			if next, ok := addInt64(creditTotal, line.AmountMinor); ok {
				creditTotal = next
			} else {
				reasons = append(reasons, ReasonAmountOverflow)
			}
		default:
			reasons = append(reasons, ReasonAdjustmentMismatch)
		}
	}
	if !debitSeen || !creditSeen || debitTotal != expected || creditTotal != expected {
		reasons = append(reasons, ReasonAdjustmentMismatch)
	}
	return reasons
}

// trimNonEmpty normalizes a ledger ID set the same way the service does:
// whitespace is trimmed, empty entries are dropped, and duplicates collapse.
func trimNonEmpty(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		set[value] = struct{}{}
	}
	return set
}
