package integrity

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// Status is the only persisted run/check state. There is no skipped status: a
// check that skips some inputs still has StatusPassed and carries the skip in
// Skipped.
type Status string

const (
	// StatusPassed means no inconsistency was detected by the check.
	StatusPassed Status = "passed"
	// StatusFailed means at least one problem or a check error was recorded.
	StatusFailed Status = "failed"
)

// CheckName identifies one of the five frozen checks.
type CheckName string

const (
	// CheckLedgerBalance independently verifies per-transaction balance and
	// that every transaction has entries.
	CheckLedgerBalance CheckName = "ledger_balance"
	// CheckValueConservation verifies the FIN-52 value equation per source and
	// currency.
	CheckValueConservation CheckName = "value_conservation"
	// CheckAppliedProposalTrail maps the stable internal/invariants contract.
	CheckAppliedProposalTrail CheckName = "applied_proposal_trail"
	// CheckAuditChain verifies the audit hash chain from genesis.
	CheckAuditChain CheckName = "audit_chain"
	// CheckTieout verifies every scope's latest statement at or before the run
	// date.
	CheckTieout CheckName = "tieout"
)

// CheckOrder is the frozen execution and report order. A completed run always
// has exactly len(CheckOrder) results in this order.
var CheckOrder = []CheckName{
	CheckLedgerBalance,
	CheckValueConservation,
	CheckAppliedProposalTrail,
	CheckAuditChain,
	CheckTieout,
}

// Problem codes shared across checks.
const (
	// CodeCheckError marks a check that could not complete its own work. The
	// check continues after its savepoint is rolled back.
	CodeCheckError = "check_error"
	// CodeLedgerTxnUnbalanced marks a transaction whose debit-credit net is
	// non-zero for one currency.
	CodeLedgerTxnUnbalanced = "ledger_txn_unbalanced"
	// CodeLedgerTxnNoEntries marks a transaction with no ledger entries.
	CodeLedgerTxnNoEntries = "ledger_txn_no_entries"
	// CodeValueConservationMismatch marks a source/currency whose accounted
	// value differs from ingested value.
	CodeValueConservationMismatch = "value_conservation_mismatch"
	// CodeAuditChainProblem maps one internal/audit verification finding.
	CodeAuditChainProblem = "audit_chain_problem"
	// CodeTieoutUnexplained marks a scope whose tie-out has a non-zero
	// unexplained amount.
	CodeTieoutUnexplained = "tieout_unexplained"
	// CodeTieoutError marks a scope whose tie-out could not be evaluated.
	CodeTieoutError = "tieout_error"
)

// Persistence and execution constants.
const (
	// MaxPersistedProblems caps the problems array in each check result. The
	// true pre-cap count is reported in ProblemCount and Truncated.
	MaxPersistedProblems = 100
	// DefaultRunTimeout bounds one run. A caller-provided shorter deadline
	// wins; operations may revisit the constant only with measured data.
	DefaultRunTimeout = 5 * time.Minute
	// AdvisoryLockKey is the single documented transaction-scoped lock key.
	// It is a stable int64 rather than a hash so versions cannot collide.
	AdvisoryLockKey int64 = 7305060001
	// outboxTopic is the durable alert topic for a failed run.
	outboxTopic = "integrity.failed"
	// maxDetailLen is the maximum number of characters in a Problem.Detail.
	maxDetailLen = 256
	// dateLayout is the wire and DB date format (UTC business date).
	dateLayout = "2006-01-02"
)

// ErrAlreadyRunning is returned when another integrity run holds the
// transaction-scoped advisory lock. It is not a finding and produces no row,
// alert, or metric change.
var ErrAlreadyRunning = errors.New("integrity: another run is already in progress")

// Date is a UTC business date rendered as YYYY-MM-DD in JSON and never as an
// RFC3339 timestamp.
type Date struct {
	time.Time
}

// NewDate normalizes t to the UTC calendar date at midnight.
func NewDate(t time.Time) Date {
	year, month, day := t.UTC().Date()
	return Date{Time: time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// ParseDate parses a strict YYYY-MM-DD date in UTC.
func ParseDate(value string) (Date, error) {
	parsed, err := time.ParseInLocation(dateLayout, value, time.UTC)
	if err != nil {
		return Date{}, fmt.Errorf("integrity: invalid date %q: %w", value, err)
	}
	return Date{Time: parsed}, nil
}

// IsZero reports whether the date is unset.
func (d Date) IsZero() bool { return d.Time.IsZero() }

// String renders the UTC date as YYYY-MM-DD (empty when unset).
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Time.UTC().Format(dateLayout)
}

// MarshalJSON encodes the date as a JSON string in YYYY-MM-DD form.
func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON accepts only a YYYY-MM-DD string.
func (d *Date) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := ParseDate(value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Problem is one finding. All four fields are always encoded; Detail is
// single-line, bounded, and redacted operator text that consumers must not
// parse for machine logic.
type Problem struct {
	Code   string `json:"code"`
	Entity string `json:"entity"`
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// CheckResult is one check's stable wire result.
type CheckResult struct {
	Name         CheckName `json:"name"`
	Status       Status    `json:"status"`
	Checked      int64     `json:"checked"`
	Skipped      int64     `json:"skipped"`
	ProblemCount int       `json:"problem_count"`
	Truncated    bool      `json:"truncated"`
	Problems     []Problem `json:"problems"`
}

// Report is one completed run. RunID is the inserted integrity_runs.id and is
// part of the CLI/worker envelope, not part of the persisted checks JSON.
type Report struct {
	RunID        string        `json:"run_id"`
	RunDate      Date          `json:"run_date"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Status       Status        `json:"status"`
	CheckCount   int           `json:"check_count"`
	FailureCount int           `json:"failure_count"`
	Checks       []CheckResult `json:"checks"`
}

// Passed reports whether the run completed with no failed checks.
func (r Report) Passed() bool { return r.Status == StatusPassed }

// Trigger records how the run was started.
type Trigger string

const (
	// TriggerSchedule is written by the daily worker scheduler.
	TriggerSchedule Trigger = "schedule"
	// TriggerManual is written by `recon integrity run`.
	TriggerManual Trigger = "manual"
)

// Options configures one run.
type Options struct {
	// RunDate is the UTC business date. The zero value means the clock's
	// current UTC date.
	RunDate Date
	// TriggeredBy is "schedule" or "manual"; empty defaults to manual.
	TriggeredBy Trigger
	// Clock is the injected time source. Nil defaults to clock.Real.
	Clock clock.Clock
	// Observer receives the completed pass/fail result after COMMIT only.
	Observer Observer
	// TieoutObserver receives per-scope tie-out measurements buffered during
	// the run and replayed after COMMIT only. Nil disables observation.
	TieoutObserver tieout.Observer
	// Timeout shortens the run. Zero, negative, or a longer value uses
	// DefaultRunTimeout; an earlier parent deadline always wins.
	Timeout time.Duration
}

// Result is a completed run: the inserted row id and its report.
type Result struct {
	ID     string
	Report Report
}

// statusFor derives the run status from the failed-check count.
func statusFor(failureCount int) Status {
	if failureCount > 0 {
		return StatusFailed
	}
	return StatusPassed
}

// finalizeCheckResult sorts problems by (code, entity, id, detail), caps them
// at MaxPersistedProblems, and derives status/count/truncated.
func finalizeCheckResult(name CheckName, checked, skipped int64, problems []Problem) CheckResult {
	if problems == nil {
		problems = []Problem{}
	}
	for i := range problems {
		problems[i].Detail = boundedDetail(problems[i].Detail)
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].Code != problems[j].Code {
			return problems[i].Code < problems[j].Code
		}
		if problems[i].Entity != problems[j].Entity {
			return problems[i].Entity < problems[j].Entity
		}
		if problems[i].ID != problems[j].ID {
			return problems[i].ID < problems[j].ID
		}
		return problems[i].Detail < problems[j].Detail
	})
	problemCount := len(problems)
	retained := problems
	if len(retained) > MaxPersistedProblems {
		retained = retained[:MaxPersistedProblems]
	}
	result := CheckResult{
		Name:         name,
		Status:       StatusPassed,
		Checked:      checked,
		Skipped:      skipped,
		ProblemCount: problemCount,
		Truncated:    problemCount > len(retained),
		Problems:     append([]Problem{}, retained...),
	}
	if problemCount > 0 {
		result.Status = StatusFailed
	}
	return result
}

// checkErrorResult converts a check failure into the stable check_error
// problem shape: checked=0, one problem, failed status.
func checkErrorResult(name CheckName, cause string) CheckResult {
	return finalizeCheckResult(name, 0, 0, []Problem{{
		Code:   CodeCheckError,
		Entity: string(name),
		ID:     "",
		Detail: boundedDetail(cause),
	}})
}

// deriveReport sets the run-level fields from the ordered check results.
func deriveReport(runDate Date, startedAt, finishedAt time.Time, checks []CheckResult) Report {
	failureCount := 0
	for _, check := range checks {
		if check.Status == StatusFailed {
			failureCount++
		}
	}
	started := startedAt.UTC()
	finished := finishedAt.UTC()
	if finished.Before(started) {
		finished = started
	}
	return Report{
		RunDate:      runDate,
		StartedAt:    started,
		FinishedAt:   finished,
		Status:       statusFor(failureCount),
		CheckCount:   len(checks),
		FailureCount: failureCount,
		Checks:       checks,
	}
}

// validate enforces the frozen report contract before persistence. Any mismatch
// is a programming error and aborts the run without a row.
func (r Report) validate() error {
	if r.CheckCount != len(CheckOrder) || len(r.Checks) != len(CheckOrder) {
		return fmt.Errorf("integrity: report has %d checks and check_count %d, want %d",
			len(r.Checks), r.CheckCount, len(CheckOrder))
	}
	for i, name := range CheckOrder {
		if r.Checks[i].Name != name {
			return fmt.Errorf("integrity: check %d is %q, want %q", i, r.Checks[i].Name, name)
		}
	}
	failureCount := 0
	for _, check := range r.Checks {
		switch check.Status {
		case StatusPassed:
			if check.ProblemCount != 0 {
				return fmt.Errorf("integrity: passed check %q has %d problems", check.Name, check.ProblemCount)
			}
		case StatusFailed:
			failureCount++
		default:
			return fmt.Errorf("integrity: check %q has invalid status %q", check.Name, check.Status)
		}
		if check.Problems == nil {
			return fmt.Errorf("integrity: check %q has a nil problems array", check.Name)
		}
		if check.ProblemCount > MaxPersistedProblems && !check.Truncated {
			return fmt.Errorf("integrity: check %q is uncapped but not truncated", check.Name)
		}
		if check.ProblemCount != len(check.Problems) && !check.Truncated {
			return fmt.Errorf("integrity: check %q problem_count %d does not match %d problems",
				check.Name, check.ProblemCount, len(check.Problems))
		}
		if check.Truncated && check.ProblemCount <= len(check.Problems) {
			return fmt.Errorf("integrity: check %q is truncated with %d/%d problems",
				check.Name, len(check.Problems), check.ProblemCount)
		}
	}
	if failureCount != r.FailureCount {
		return fmt.Errorf("integrity: failure_count %d does not match %d failed checks", r.FailureCount, failureCount)
	}
	if r.Status != statusFor(failureCount) {
		return fmt.Errorf("integrity: status %q does not match failure_count %d", r.Status, failureCount)
	}
	if r.FinishedAt.Before(r.StartedAt) {
		return fmt.Errorf("integrity: finished_at is before started_at")
	}
	if r.RunDate.IsZero() {
		return fmt.Errorf("integrity: run_date is required")
	}
	return nil
}

// boundedDetail collapses whitespace/control characters into single spaces and
// truncates to at most maxDetailLen characters on a rune boundary. It is the
// single redaction point for every persisted problem detail.
func boundedDetail(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for _, r := range value {
		if r == utf8.RuneError {
			continue
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			if !space && builder.Len() > 0 {
				builder.WriteByte(' ')
				space = true
			}
			continue
		}
		builder.WriteRune(r)
		space = false
	}
	result := strings.TrimSpace(builder.String())
	if utf8.RuneCountInString(result) <= maxDetailLen {
		return result
	}
	runes := []rune(result)
	return string(runes[:maxDetailLen])
}
