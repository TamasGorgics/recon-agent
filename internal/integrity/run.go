package integrity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/outbox"
	"github.com/TamasGorgics/recon-agent/internal/tieout"
)

// DB is the transaction surface Run needs. *pgxpool.Pool and *pgx.Conn satisfy
// it. Run starts a read-write transaction and then promotes it to REPEATABLE
// READ before the first query, so a narrow Begin-only interface still yields
// the frozen snapshot semantics.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Run executes the five checks in CheckOrder in one REPEATABLE READ snapshot,
// inserts exactly one integrity_runs row, and (for a failed run) enqueues
// exactly one integrity.failed outbox alert in the same transaction. It returns
// (Result, nil) for a completed failure; a non-nil error means the run could
// not complete (lock loser, begin/query/insert/commit error, cancellation, or
// panic), in which case no row, alert, or observer notification exists.
func Run(ctx context.Context, database DB, opts Options) (result Result, err error) {
	if database == nil {
		return Result{}, errors.New("integrity: nil database")
	}
	opts, err = normalizeOptions(opts)
	if err != nil {
		return Result{}, err
	}

	startedAt := opts.Clock.Now().UTC()
	runDate := opts.RunDate
	if runDate.IsZero() {
		runDate = NewDate(startedAt)
	} else {
		runDate = NewDate(runDate.Time)
	}
	if runDate.IsZero() {
		return Result{}, errors.New("integrity: run date could not be determined")
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var tx pgx.Tx
	txDone := false
	defer func() {
		if tx != nil && !txDone {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			if !txDone {
				result = Result{}
			}
			err = fmt.Errorf("integrity: run panicked: %s", boundedDetail(fmt.Sprint(recovered)))
		}
	}()

	tx, err = database.Begin(runCtx)
	if err != nil {
		return Result{}, fmt.Errorf("integrity: begin: %w", err)
	}
	if _, err := tx.Exec(runCtx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ WRITE`); err != nil {
		return Result{}, fmt.Errorf("integrity: configure snapshot: %w", err)
	}

	var locked bool
	if err := tx.QueryRow(runCtx, `SELECT pg_try_advisory_xact_lock($1)`, AdvisoryLockKey).Scan(&locked); err != nil {
		return Result{}, fmt.Errorf("integrity: acquire advisory lock: %w", err)
	}
	if !locked {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		txDone = true
		return Result{}, ErrAlreadyRunning
	}

	buffer := newTieoutBuffer(opts.TieoutObserver)
	var tieoutObserver tieout.Observer
	if buffer != nil {
		tieoutObserver = buffer
	}
	checkOpts := checkOptions{RunDate: runDate, TieoutObserver: tieoutObserver}

	checks := make([]CheckResult, 0, len(CheckOrder))
	for index, name := range CheckOrder {
		checkResult, checkErr := runCheck(runCtx, tx, index, name, checkOpts)
		if checkErr != nil {
			return Result{}, checkErr
		}
		checks = append(checks, checkResult)
	}

	report := deriveReport(runDate, startedAt, opts.Clock.Now().UTC(), checks)
	if err := report.validate(); err != nil {
		return Result{}, err
	}
	checksJSON, err := json.Marshal(report.Checks)
	if err != nil {
		return Result{}, fmt.Errorf("integrity: encode checks: %w", err)
	}

	var runID string
	if err := tx.QueryRow(runCtx, `
		INSERT INTO integrity_runs
		    (run_date, started_at, finished_at, status, check_count,
		     failure_count, checks, triggered_by)
		VALUES ($1::date, $2, $3, $4, $5, $6, $7::jsonb, $8)
		RETURNING id::text`,
		report.RunDate.String(), report.StartedAt, report.FinishedAt,
		string(report.Status), report.CheckCount, report.FailureCount,
		string(checksJSON), string(opts.TriggeredBy)).Scan(&runID); err != nil {
		return Result{}, fmt.Errorf("integrity: insert run: %w", err)
	}

	if report.Status == StatusFailed {
		payload, err := json.Marshal(failurePayload(runID, report, opts.TriggeredBy))
		if err != nil {
			return Result{}, fmt.Errorf("integrity: encode failure alert: %w", err)
		}
		if _, err := outbox.EnqueueAggregate(runCtx, tx, "integrity_run", runID,
			outboxTopic, json.RawMessage(payload), outboxTopic+":"+runID); err != nil {
			return Result{}, fmt.Errorf("integrity: enqueue failure alert: %w", err)
		}
	}

	if err := tx.Commit(runCtx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		txDone = true
		return Result{}, fmt.Errorf("integrity: commit: %w", err)
	}
	txDone = true

	report.RunID = runID
	result = Result{ID: runID, Report: report}

	// Post-commit only. Neither observer nor the log can change the persisted
	// outcome; the row is authoritative if any of them is skipped.
	logCompleted(report, opts.TriggeredBy)
	buffer.flush()
	notifyRunObserver(opts.Observer, report.Passed())
	return result, nil
}

// normalizeOptions applies defaults and rejects unknown trigger values.
func normalizeOptions(opts Options) (Options, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.TriggeredBy == "" {
		opts.TriggeredBy = TriggerManual
	}
	switch opts.TriggeredBy {
	case TriggerSchedule, TriggerManual:
	default:
		return Options{}, fmt.Errorf("integrity: invalid triggered_by %q", opts.TriggeredBy)
	}
	if opts.Timeout <= 0 || opts.Timeout > DefaultRunTimeout {
		opts.Timeout = DefaultRunTimeout
	}
	return opts, nil
}

// runCheck runs one check inside a savepoint. A check error rolls back to the
// savepoint and becomes a check_error problem, so the outer transaction stays
// usable and the remaining checks plus the final row/alert can complete.
func runCheck(ctx context.Context, tx pgx.Tx, index int, name CheckName, opts checkOptions) (CheckResult, error) {
	savepoint := fmt.Sprintf("integrity_check_%d", index+1)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
		return CheckResult{}, fmt.Errorf("integrity: savepoint %s: %w", savepoint, err)
	}

	result, err := executeCheck(ctx, tx, name, opts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); rollbackErr != nil {
			return CheckResult{}, fmt.Errorf("integrity: rollback to %s: %w", savepoint, rollbackErr)
		}
		result = checkErrorResult(name, errorCause(err))
	}

	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+savepoint); err != nil {
		return CheckResult{}, fmt.Errorf("integrity: release savepoint %s: %w", savepoint, err)
	}
	return result, nil
}

// logCompleted emits the post-commit structured summary. A panicking log
// handler is swallowed: the persisted row is already the source of truth.
func logCompleted(report Report, trigger Trigger) {
	defer func() {
		_ = recover()
	}()
	slog.Default().Info("integrity run completed",
		"run_id", report.RunID,
		"run_date", report.RunDate.String(),
		"status", string(report.Status),
		"check_count", report.CheckCount,
		"failure_count", report.FailureCount,
		"triggered_by", string(trigger),
	)
}

// alertPayload is the bounded integrity.failed outbox payload: names and
// counts only, never problem details, entity ids, amounts, or raw data.
type alertPayload struct {
	RunID         string         `json:"run_id"`
	RunDate       string         `json:"run_date"`
	Status        Status         `json:"status"`
	CheckCount    int            `json:"check_count"`
	FailureCount  int            `json:"failure_count"`
	FailedChecks  []CheckName    `json:"failed_checks"`
	ProblemCounts map[string]int `json:"problem_counts"`
	TriggeredBy   Trigger        `json:"triggered_by"`
}

func failurePayload(runID string, report Report, trigger Trigger) alertPayload {
	failed := make([]CheckName, 0, report.FailureCount)
	counts := make(map[string]int, report.FailureCount)
	for _, check := range report.Checks {
		if check.Status != StatusFailed {
			continue
		}
		failed = append(failed, check.Name)
		counts[string(check.Name)] = check.ProblemCount
	}
	return alertPayload{
		RunID:         runID,
		RunDate:       report.RunDate.String(),
		Status:        report.Status,
		CheckCount:    report.CheckCount,
		FailureCount:  report.FailureCount,
		FailedChecks:  failed,
		ProblemCounts: counts,
		TriggeredBy:   trigger,
	}
}
