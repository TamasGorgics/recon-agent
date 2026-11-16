package limits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/policy"
)

const (
	// breakerActorID is the id half of system:circuit-breaker.
	breakerActorID = "circuit-breaker"
	// dailyCapBasisPoints is the denominator used for the integer break-rate
	// comparison, so no floating point enters a money/limit decision.
	dailyCapBasisPoints int64 = 10_000
)

var (
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	// canonicalUUIDPattern accepts only the lower-case canonical textual UUID
	// form, so an upper-case or otherwise non-canonical candidate ID can never
	// reach the uuid cast in the cumulative query.
	canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// NewService validates the dependencies and the effective limits config
// (defaults applied) and returns a read-mostly check service.
func NewService(opts Options) (*Service, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidOptions)
	}
	if opts.Clock == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidOptions)
	}
	cfg := opts.Config.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOptions, err)
	}
	return &Service{pool: opts.Pool, clock: opts.Clock, config: cfg}, nil
}

// Check applies the FIN-53 guard to one candidate.
//
// T1/T2 candidates pass through with Allowed=true and reason not_t0; any other
// tier, including the empty value, fails closed to T1 with reason invalid_tier.
// A T0 candidate is failed closed to T1 with a machine reason when its value or
// currency is invalid, a non-empty proposal ID is not a canonical lower-case
// UUID, the switch is disabled, its currency has no configured positive cap,
// its payload cannot be read unambiguously, or its value would exceed the
// UTC-day cumulative cap. A break-rate spike or daily-cap hit also trips the
// breaker once. Database failures are returned as errors.
//
// Check never reserves or persists the candidate. The cumulative query counts
// only already-persisted auto_approved or applied proposals, so a caller must
// persist each auto-approved result before checking the next candidate, or
// sequential checks can each pass below the cap. A non-empty ProposalID is
// excluded from the cumulative sum so a candidate whose row is already
// persisted is counted once via ValueMinor, not twice.
func (s *Service) Check(ctx context.Context, candidate Candidate) (Result, error) {
	switch candidate.Tier {
	case policy.TierT0:
	case policy.TierT1, policy.TierT2:
		return Result{Allowed: true, Tier: candidate.Tier, Reason: ReasonNotT0}, nil
	default:
		return failedClosed(ReasonInvalidTier, 0, 0, false), nil
	}
	if !currencyPattern.MatchString(candidate.Currency) || candidate.ValueMinor < 0 {
		return failedClosed(ReasonValueUnknown, 0, 0, false), nil
	}
	if candidate.ProposalID != "" && !canonicalUUIDPattern.MatchString(candidate.ProposalID) {
		return failedClosed(ReasonLimitsUnavailable, 0, 0, false), nil
	}

	enabled, err := s.switchEnabled(ctx)
	if err != nil {
		return Result{}, err
	}
	if !enabled {
		return failedClosed(ReasonAutoApplyDisabled, 0, 0, false), nil
	}

	dayStart := utcDayStart(s.clock.Now())
	dayEnd := dayStart.Add(24 * time.Hour)
	breaks, matches, err := s.breakCounts(ctx, dayStart, dayEnd)
	if err != nil {
		return Result{}, err
	}
	if breakRateExceeded(breaks, matches, s.config.BreakRateThresholdBPS, s.config.BreakRateMinSample) {
		tripped, err := s.TripBreaker(ctx, ReasonBreakRateSpike)
		if err != nil {
			return Result{}, err
		}
		return failedClosed(ReasonBreakRateSpike, 0, 0, false, tripped), nil
	}

	cumulative, err := s.cumulativeMinor(ctx, dayStart, candidate.Currency, candidate.ProposalID)
	if err != nil {
		if errors.Is(err, errMalformedPayload) {
			return failedClosed(ReasonLimitsUnavailable, 0, 0, true), nil
		}
		return Result{}, err
	}

	capMinor, ok := s.config.DailyMaxValueMinor[candidate.Currency]
	if !ok || capMinor <= 0 {
		return failedClosed(ReasonCurrencyCapMissing, cumulative, 0, true), nil
	}
	if exceedsDailyCap(cumulative, candidate.ValueMinor, capMinor) {
		tripped, err := s.TripBreaker(ctx, ReasonDailyCapExceeded)
		if err != nil {
			return Result{}, err
		}
		return failedClosed(ReasonDailyCapExceeded, cumulative, capMinor, false, tripped), nil
	}
	return Result{
		Allowed:         true,
		Tier:            policy.TierT0,
		Reason:          ReasonWithinLimits,
		CumulativeMinor: cumulative,
		CapMinor:        capMinor,
		SwitchEnabled:   true,
	}, nil
}

// TripBreaker disables auto_apply_enabled with the system circuit-breaker
// actor and one switch.set audit entry. It locks the switch row, so concurrent
// trips produce exactly one update/audit; already-disabled is a no-op that
// returns false.
func (s *Service) TripBreaker(ctx context.Context, reason string) (bool, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false, ErrTripReason
	}
	actor, err := audit.SystemActor(breakerActorID)
	if err != nil {
		return false, fmt.Errorf("limits: build circuit-breaker actor: %w", err)
	}
	updatedAt := s.clock.Now().UTC().Truncate(time.Microsecond)

	tripped := false
	err = db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		// WithTx may retry after a serialization failure; never carry a
		// previous attempt's result forward.
		tripped = false

		var (
			enabled   bool
			oldReason string
		)
		err := tx.QueryRow(ctx,
			`SELECT enabled, reason
			   FROM system_switches
			  WHERE name = $1
			  FOR UPDATE`, SwitchAutoApply).
			Scan(&enabled, &oldReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrSwitchMissing, SwitchAutoApply)
		}
		if err != nil {
			return fmt.Errorf("limits: lock auto-apply switch: %w", err)
		}
		if !enabled {
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE system_switches
			    SET enabled = false, reason = $2, updated_by = $3, updated_at = $4
			  WHERE name = $1`,
			SwitchAutoApply, reason, CircuitBreakerActor, updatedAt); err != nil {
			return fmt.Errorf("limits: disable auto-apply switch: %w", err)
		}
		if _, err := audit.Append(ctx, tx, audit.Entry{
			Actor:      actor,
			Action:     ActionSwitchSet,
			EntityType: EntitySystemSwitch,
			EntityID:   SwitchAutoApply,
			Before: map[string]any{
				"enabled": true,
				"reason":  oldReason,
			},
			After: map[string]any{
				"enabled": false,
				"reason":  reason,
			},
			Timestamp: updatedAt,
		}); err != nil {
			return fmt.Errorf("limits: append circuit-breaker audit: %w", err)
		}
		tripped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return tripped, nil
}

func (s *Service) switchEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT enabled FROM system_switches WHERE name = $1`, SwitchAutoApply).
		Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrSwitchMissing, SwitchAutoApply)
	}
	if err != nil {
		return false, fmt.Errorf("limits: read auto-apply switch: %w", err)
	}
	return enabled, nil
}

// breakCounts returns the UTC-day break count and active-match count. Break
// status is intentionally not filtered: every break detected in the day is an
// anomaly signal.
func (s *Service) breakCounts(ctx context.Context, dayStart, dayEnd time.Time) (int64, int64, error) {
	var breaks, matches int64
	if err := s.pool.QueryRow(ctx,
		`SELECT
		     (SELECT count(*) FROM breaks
		       WHERE detected_at >= $1 AND detected_at < $2),
		     (SELECT count(*) FROM matches
		       WHERE status = 'active'
		         AND created_at >= $1 AND created_at < $2)`,
		dayStart, dayEnd).Scan(&breaks, &matches); err != nil {
		return 0, 0, fmt.Errorf("limits: read break-rate window: %w", err)
	}
	return breaks, matches, nil
}

// cumulativeMinor sums debit lines in currency for T0 proposals that are
// auto-approved or applied today (created or applied in the current UTC day).
// When excludeProposalID is non-empty that proposal's row is left out, so a
// candidate whose row is already persisted is counted once via ValueMinor
// instead of twice. The raw lines are parsed in Go so malformed payloads fail
// closed instead of relying on a SQL cast error.
func (s *Service) cumulativeMinor(ctx context.Context, dayStart time.Time, currency, excludeProposalID string) (int64, error) {
	query := `SELECT COALESCE(p.payload->'lines', 'null'::jsonb)
		   FROM proposals p
		   LEFT JOIN applied_proposals ap ON ap.proposal_id = p.id
		  WHERE p.risk_tier = 'T0'
		    AND p.status IN ('auto_approved', 'applied')
		    AND (p.created_at >= $1 OR ap.applied_at >= $1)`
	args := []any{dayStart}
	if excludeProposalID != "" {
		query += `
		    AND p.id <> $2::uuid`
		args = append(args, excludeProposalID)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("limits: read daily cumulative: %w", err)
	}
	defer rows.Close()

	var cumulative int64
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, fmt.Errorf("limits: scan daily cumulative: %w", err)
		}
		amount, err := sumDebitLines(raw, currency)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", errMalformedPayload, err)
		}
		if amount > 0 && cumulative > math.MaxInt64-amount {
			return 0, fmt.Errorf("%w: cumulative minor overflow", errMalformedPayload)
		}
		cumulative += amount
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("limits: iterate daily cumulative: %w", err)
	}
	return cumulative, nil
}

// sumDebitLines sums amount_minor for debit lines in currency. A missing or
// JSON-null lines value (link/duplicate proposals) is zero. Any non-array
// lines value, non-object line, missing/non-integer/negative amount, or
// missing currency/direction field is malformed.
func sumDebitLines(raw []byte, currency string) (int64, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, nil
	}
	var lines []json.RawMessage
	if err := json.Unmarshal(raw, &lines); err != nil {
		return 0, fmt.Errorf("lines are not an array: %w", err)
	}
	var total int64
	for i, rawLine := range lines {
		var line map[string]json.RawMessage
		if err := json.Unmarshal(rawLine, &line); err != nil || line == nil {
			return 0, fmt.Errorf("line %d is not an object: %v", i, err)
		}
		amountRaw, ok := line["amount_minor"]
		if !ok {
			return 0, fmt.Errorf("line %d is missing amount_minor", i)
		}
		amountRaw = bytes.TrimSpace(amountRaw)
		if len(amountRaw) == 0 || (amountRaw[0] != '-' && (amountRaw[0] < '0' || amountRaw[0] > '9')) {
			return 0, fmt.Errorf("line %d amount_minor is not an integer JSON number", i)
		}
		var amount int64
		if err := json.Unmarshal(amountRaw, &amount); err != nil {
			return 0, fmt.Errorf("line %d amount_minor is not an integer: %w", i, err)
		}
		currencyRaw, ok := line["currency"]
		if !ok {
			return 0, fmt.Errorf("line %d is missing currency", i)
		}
		var lineCurrency string
		if err := json.Unmarshal(currencyRaw, &lineCurrency); err != nil || lineCurrency == "" {
			return 0, fmt.Errorf("line %d currency is missing or not a string", i)
		}
		directionRaw, ok := line["direction"]
		if !ok {
			return 0, fmt.Errorf("line %d is missing direction", i)
		}
		var direction string
		if err := json.Unmarshal(directionRaw, &direction); err != nil || direction == "" {
			return 0, fmt.Errorf("line %d direction is missing or not a string", i)
		}
		if amount < 0 {
			return 0, fmt.Errorf("line %d amount_minor %d is negative", i, amount)
		}
		if direction != "debit" || lineCurrency != currency {
			continue
		}
		if amount > 0 && total > math.MaxInt64-amount {
			return 0, fmt.Errorf("line %d amount overflows the cumulative total", i)
		}
		total += amount
	}
	return total, nil
}

// exceedsDailyCap reports whether a candidate must not be auto-approved. A
// candidate exactly at the cap still fits; a zero-value candidate at the cap
// does not (the day already consumed the full cap).
func exceedsDailyCap(cumulative, value, capMinor int64) bool {
	if cumulative >= capMinor {
		return true
	}
	return value > capMinor-cumulative
}

// breakRateExceeded compares breaks/(breaks+active matches) to the threshold in
// basis points with integer arithmetic. The minimum sample applies to the total
// event count, so a tiny window can never trip the breaker.
func breakRateExceeded(breaks, matches int64, thresholdBPS, minSample int) bool {
	total := breaks + matches
	if total < int64(minSample) {
		return false
	}
	return breaks*dailyCapBasisPoints >= total*int64(thresholdBPS)
}

func failedClosed(reason string, cumulative, capMinor int64, switchEnabled bool, tripped ...bool) Result {
	breakerTripped := false
	if len(tripped) > 0 {
		breakerTripped = tripped[0]
	}
	return Result{
		Allowed:         false,
		Tier:            policy.TierT1,
		Reason:          reason,
		CumulativeMinor: cumulative,
		CapMinor:        capMinor,
		BreakerTripped:  breakerTripped,
		SwitchEnabled:   switchEnabled,
	}
}

func utcDayStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
