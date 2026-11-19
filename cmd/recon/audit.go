package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
)

const auditUsage = `usage: recon audit <command> [arguments]

Commands:
  verify   verify the audit hash chain without writing
           usage: recon audit verify [--from <seq>] [--json]
           --from defaults to 1; values below 1 are clamped to 1
           exit: 0 intact, 1 findings, 2 usage/config/connection/query error
`

// runAudit dispatches the minimal `recon audit` group.
func runAudit(ctx context.Context, args []string) error {
	return runAuditTo(ctx, args, os.Stdout)
}

// runAuditTo is runAudit with a testable writer and dispatch.
func runAuditTo(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return &exitError{code: 2, err: errors.New(
			"audit: missing command (try: recon audit verify)")}
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(out, auditUsage)
		return err
	case "verify":
		return runAuditVerifyTo(ctx, args[1:], out)
	default:
		return &exitError{code: 2, err: fmt.Errorf(
			"unknown audit command %q (try: recon audit verify)", args[0])}
	}
}

// auditVerifyTxOptions is the single snapshot the whole verification runs in.
// REPEATABLE READ keeps the head and the rows consistent, so a concurrent
// append cannot fake a truncated tail or a head mismatch; READ ONLY makes a
// verifier write fail with a database error instead of committing (spec 3.5).
var auditVerifyTxOptions = pgx.TxOptions{
	IsoLevel:   pgx.RepeatableRead,
	AccessMode: pgx.ReadOnly,
}

// withAuditVerifyTx runs fn inside the verification transaction and always
// rolls back. It never commits: verification must not write.
func withAuditVerifyTx(ctx context.Context, b db.Beginner,
	fn func(context.Context, pgx.Tx) error) error {
	tx, err := b.BeginTx(ctx, auditVerifyTxOptions)
	if err != nil {
		return err
	}
	// Detach from cancellation so the rollback still happens when ctx expires.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return fn(ctx, tx)
}

// runAuditVerifyTo implements `recon audit verify [--from <seq>] [--json]`.
func runAuditVerifyTo(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	from := flags.Int64("from", 1, "first audit seq to verify")
	asJSON := flags.Bool("json", false, "print one JSON object")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, auditUsage)
			return writeErr
		}
		return auditVerifyUsageError("%v", err)
	}
	if flags.NArg() != 0 {
		return auditVerifyUsageError("unexpected argument %q", flags.Arg(0))
	}
	// Spec 3.1: numeric values below 1 clamp to 1 without an error.
	if *from < 1 {
		*from = 1
	}

	// Verification needs only the application-role DSN. Do not require the
	// migration database.url, policy file, or unrelated runtime settings.
	cfg, err := config.Load()
	if err != nil {
		return &exitError{code: 2, err: fmt.Errorf("audit verify: %w", err)}
	}
	if strings.TrimSpace(cfg.Database.AppURL) == "" {
		return &exitError{code: 2, err: errors.New(
			"config: database.app_url is required for recon audit verify (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")}
	}
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return &exitError{code: 2, err: fmt.Errorf(
			"audit verify: open application role: %w", err)}
	}
	defer app.Close()

	var report audit.Report
	err = withAuditVerifyTx(ctx, app.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var verifyErr error
		report, verifyErr = audit.Verify(ctx, tx, *from)
		return verifyErr
	})
	if err != nil {
		if errors.Is(err, audit.ErrFromSeqBeyondTail) {
			return &exitError{code: 2, err: fmt.Errorf(
				"audit verify: --from is beyond the chain tail: %w", err)}
		}
		return &exitError{code: 2, err: fmt.Errorf("audit verify: %w", err)}
	}

	result := newAuditVerifyResult(report)
	if *asJSON {
		if err := writeAuditVerifyJSON(out, result); err != nil {
			return &exitError{code: 2, err: err}
		}
	} else if err := writeAuditVerifyHuman(out, result); err != nil {
		return &exitError{code: 2, err: err}
	}

	if len(result.Findings) > 0 {
		return &exitError{code: 1, err: fmt.Errorf(
			"audit verify: %d finding(s)", len(result.Findings))}
	}
	return nil
}

// auditVerifyJSON is the exact `--json` shape from spec 3.3.
type auditVerifyJSON struct {
	From     int64              `json:"from"`
	HeadSeq  int64              `json:"head_seq"`
	Checked  int                `json:"checked"`
	Findings []auditFindingJSON `json:"findings"`
}

type auditFindingJSON struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// newAuditVerifyResult renders a report into the stable output shape:
// findings sorted by (seq, kind) and an empty (never null) findings array.
func newAuditVerifyResult(report audit.Report) auditVerifyJSON {
	result := auditVerifyJSON{
		From:     report.FromSeq,
		HeadSeq:  report.HeadSeq,
		Checked:  auditVerifyChecked(report),
		Findings: make([]auditFindingJSON, 0, len(report.Problems)),
	}
	for _, problem := range report.Problems {
		result.Findings = append(result.Findings, auditFindingJSON{
			Seq:    problem.Seq,
			Kind:   string(problem.Kind),
			Detail: problem.Detail,
		})
	}
	slices.SortStableFunc(result.Findings, func(a, b auditFindingJSON) int {
		if a.Seq != b.Seq {
			return cmp.Compare(a.Seq, b.Seq)
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	return result
}

// auditVerifyChecked is spec 3.3's checked value: the size of the effective
// from..head range, max(0, head_seq-from+1). It intentionally follows the head
// mirror rather than the rows that survived tampering, so a truncated chain
// still reports the range the operator asked for.
func auditVerifyChecked(report audit.Report) int {
	span := report.HeadSeq - report.FromSeq + 1
	if span <= 0 {
		return 0
	}
	return int(span)
}

func writeAuditVerifyJSON(out io.Writer, result auditVerifyJSON) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("audit verify: encode JSON: %w", err)
	}
	if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
		return fmt.Errorf("audit verify: write JSON: %w", err)
	}
	return nil
}

func writeAuditVerifyHuman(out io.Writer, result auditVerifyJSON) error {
	status := "OK"
	if len(result.Findings) > 0 {
		status = "FINDINGS"
	}
	if _, err := fmt.Fprintf(out,
		"audit verify: %s from=%d head=%d checked=%d findings=%d\n",
		status, result.From, result.HeadSeq, result.Checked,
		len(result.Findings)); err != nil {
		return fmt.Errorf("audit verify: write summary: %w", err)
	}
	for _, finding := range result.Findings {
		if _, err := fmt.Fprintf(out, "  seq=%d kind=%s detail=%s\n",
			finding.Seq, finding.Kind, finding.Detail); err != nil {
			return fmt.Errorf("audit verify: write finding: %w", err)
		}
	}
	return nil
}

// auditVerifyUsageError marks a flag/argument problem as exit 2.
func auditVerifyUsageError(format string, args ...any) error {
	return &exitError{code: 2, err: fmt.Errorf("audit verify: "+format, args...)}
}
