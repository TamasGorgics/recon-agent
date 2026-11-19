package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/traceability"
)

const traceUsage = `usage: recon trace --txn <ledger-txn-uuid> [--json]

Walks the FIN-32 chain from a ledger transaction back to the agent run,
verification, policy decision/config, approvals, executor audit, and the
ledger transaction's entries. Read-only: one REPEATABLE READ snapshot.

Exit codes:
  0   root ledger transaction found, even if the chain is partial
  1   root ledger transaction not found
  2   usage, config, connection, or query error
`

// traceSchemaVersion is the stable CLI JSON schema version. The case-file API
// has its own version (wave 4b) and is not changed here.
const traceSchemaVersion = 1

// canonicalTraceIDPattern rejects a malformed --txn before any config or
// database work, so usage errors stay cheap and exit 2.
var canonicalTraceIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// traceTxOptions is the single snapshot the whole trace runs in. READ ONLY
// makes the reader's no-write guarantee a database error if it were ever
// violated; REPEATABLE READ keeps every link consistent with the root.
var traceTxOptions = pgx.TxOptions{
	IsoLevel:   pgx.RepeatableRead,
	AccessMode: pgx.ReadOnly,
}

// withTraceTx runs fn inside the trace transaction and always rolls back. It
// never commits: tracing must not write.
func withTraceTx(ctx context.Context, b db.Beginner,
	fn func(context.Context, pgx.Tx) error) error {
	tx, err := b.BeginTx(ctx, traceTxOptions)
	if err != nil {
		return err
	}
	// Detach from cancellation so the rollback still happens when ctx expires.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return fn(ctx, tx)
}

// runTrace is runTraceTo with the process stdout.
func runTrace(ctx context.Context, args []string) error {
	return runTraceTo(ctx, args, os.Stdout)
}

// runTraceTo implements `recon trace --txn <id> [--json]`.
func runTraceTo(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("trace", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	txnID := flags.String("txn", "", "ledger transaction UUID")
	asJSON := flags.Bool("json", false, "print one JSON object")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, writeErr := fmt.Fprint(out, traceUsage)
			return writeErr
		}
		return traceUsageError("%v", err)
	}
	if flags.NArg() != 0 {
		return traceUsageError("unexpected argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*txnID) == "" {
		return traceUsageError("--txn is required")
	}
	if *txnID != strings.TrimSpace(*txnID) {
		return traceUsageError("--txn must not have surrounding whitespace")
	}
	if !canonicalTraceIDPattern.MatchString(*txnID) {
		return traceUsageError("--txn must be a canonical lowercase UUID")
	}

	// A trace needs only the application-role DSN. Flag validation happens
	// before config, so usage errors exit 2 without opening a pool.
	cfg, err := config.Load()
	if err != nil {
		return &exitError{code: 2, err: fmt.Errorf("trace: %w", err)}
	}
	if strings.TrimSpace(cfg.Database.AppURL) == "" {
		return &exitError{code: 2, err: errors.New(
			"config: database.app_url is required for recon trace (set DATABASE_APP_URL or database.app_url in CONFIG_FILE)")}
	}
	app, err := db.OpenRole(ctx, db.RoleApp, cfg.Database.AppURL)
	if err != nil {
		return &exitError{code: 2, err: fmt.Errorf(
			"trace: open application role: %w", err)}
	}
	defer app.Close()

	var trace traceability.Trace
	err = withTraceTx(ctx, app.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var traceErr error
		trace, traceErr = traceability.ByLedgerTxn(ctx, tx, *txnID)
		return traceErr
	})
	if err != nil {
		if errors.Is(err, traceability.ErrNotFound) {
			return &exitError{code: 1, err: fmt.Errorf(
				"trace: ledger txn %s not found", *txnID)}
		}
		return &exitError{code: 2, err: fmt.Errorf("trace: %w", err)}
	}

	if *asJSON {
		if err := writeTraceJSON(out, trace); err != nil {
			return &exitError{code: 2, err: err}
		}
		return nil
	}
	if err := writeTraceHuman(out, trace); err != nil {
		return &exitError{code: 2, err: err}
	}
	return nil
}

// traceJSON is the exact CLI shape: exactly four top-level fields. Chromium
// arrays are always arrays, never null.
type traceJSON struct {
	SchemaVersion int                     `json:"schema_version"`
	LedgerTxn     *traceability.LedgerTxn `json:"ledger_txn"`
	Chains        []traceability.Chain    `json:"chains"`
	Missing       []string                `json:"missing"`
}

func newTraceJSON(trace traceability.Trace) traceJSON {
	chains := trace.Chains
	if chains == nil {
		chains = []traceability.Chain{}
	}
	missing := trace.Missing
	if missing == nil {
		missing = []string{}
	}
	return traceJSON{
		SchemaVersion: traceSchemaVersion,
		LedgerTxn:     trace.LedgerTxn,
		Chains:        chains,
		Missing:       missing,
	}
}

func writeTraceJSON(out io.Writer, trace traceability.Trace) error {
	encoded, err := json.Marshal(newTraceJSON(trace))
	if err != nil {
		return fmt.Errorf("trace: encode JSON: %w", err)
	}
	if _, err := fmt.Fprintf(out, "%s\n", encoded); err != nil {
		return fmt.Errorf("trace: write JSON: %w", err)
	}
	return nil
}

func writeTraceHuman(out io.Writer, trace traceability.Trace) error {
	if trace.LedgerTxn == nil {
		return fmt.Errorf("trace: internal error: ledger txn is missing")
	}
	if _, err := fmt.Fprintf(out, "trace: ledger txn %s\n", trace.LedgerTxn.ID); err != nil {
		return fmt.Errorf("trace: write header: %w", err)
	}
	if _, err := fmt.Fprintf(out,
		"  ledger_txn: seq=%d kind=%s value_date=%s\n",
		trace.LedgerTxn.Seq, trace.LedgerTxn.Kind, trace.LedgerTxn.ValueDate); err != nil {
		return fmt.Errorf("trace: write ledger txn: %w", err)
	}
	if _, err := fmt.Fprintf(out, "  chains: %d\n", len(trace.Chains)); err != nil {
		return fmt.Errorf("trace: write chain count: %w", err)
	}
	for i := range trace.Chains {
		if err := writeTraceChainHuman(out, i+1, &trace.Chains[i]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  missing: %s\n", markersOrNone(trace.Missing)); err != nil {
		return fmt.Errorf("trace: write missing: %w", err)
	}
	return nil
}

func writeTraceChainHuman(out io.Writer, number int, chain *traceability.Chain) error {
	applied := chain.AppliedProposal
	if applied == nil {
		if _, err := fmt.Fprintf(out, "  chain %d: applied_proposal missing\n", number); err != nil {
			return fmt.Errorf("trace: write chain: %w", err)
		}
		return writeChainMissing(out, chain)
	}
	if _, err := fmt.Fprintf(out,
		"  chain %d: applied proposal %s at %s\n",
		number, applied.ProposalID,
		applied.AppliedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("trace: write chain: %w", err)
	}
	if _, err := fmt.Fprintf(out,
		"    applied_proposal: proposal=%s ledger_txn=%s executor_run=%s\n",
		applied.ProposalID, textOrNone(applied.LedgerTxnID),
		applied.ExecutorRunID); err != nil {
		return fmt.Errorf("trace: write applied proposal: %w", err)
	}

	if chain.Proposal == nil {
		if _, err := fmt.Fprint(out, "    proposal: missing\n"); err != nil {
			return fmt.Errorf("trace: write proposal: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(out,
			"    proposal: %s type=%s status=%s risk_tier=%s\n",
			chain.Proposal.ID, chain.Proposal.Type, chain.Proposal.Status,
			textOrNone(chain.Proposal.RiskTier)); err != nil {
			return fmt.Errorf("trace: write proposal: %w", err)
		}
	}

	if chain.Case == nil {
		if _, err := fmt.Fprint(out, "    case: missing\n"); err != nil {
			return fmt.Errorf("trace: write case: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(out,
			"      case: %s status=%s currency=%s value_minor=%d\n",
			chain.Case.ID, chain.Case.Status, chain.Case.Currency,
			chain.Case.ValueMinor); err != nil {
			return fmt.Errorf("trace: write case: %w", err)
		}
		if _, err := fmt.Fprintf(out, "        breaks: %d\n", len(chain.Breaks)); err != nil {
			return fmt.Errorf("trace: write break count: %w", err)
		}
		for j := range chain.Breaks {
			brk := &chain.Breaks[j]
			if _, err := fmt.Fprintf(out,
				"          break %s side=%s status=%s delta_minor=%d currency=%s\n",
				brk.ID, brk.Side, brk.Status, brk.DeltaMinor, brk.Currency); err != nil {
				return fmt.Errorf("trace: write break: %w", err)
			}
		}
	}

	if chain.AgentRun == nil {
		if _, err := fmt.Fprint(out, "      agent_run: missing\n"); err != nil {
			return fmt.Errorf("trace: write agent run: %w", err)
		}
	} else {
		traceID := "missing (agent_run.trace_id)"
		if chain.AgentRun.TraceID != nil && strings.TrimSpace(*chain.AgentRun.TraceID) != "" {
			traceID = *chain.AgentRun.TraceID
		}
		if _, err := fmt.Fprintf(out,
			"      agent_run: %s model=%s trace_id=%s\n",
			chain.AgentRun.ID, chain.AgentRun.Model, traceID); err != nil {
			return fmt.Errorf("trace: write agent run: %w", err)
		}
	}

	if len(chain.Verification) == 0 || string(chain.Verification) == "null" {
		if _, err := fmt.Fprint(out, "      verification: missing\n"); err != nil {
			return fmt.Errorf("trace: write verification: %w", err)
		}
	} else if _, err := fmt.Fprintf(out, "      verification: %s\n", chain.Verification); err != nil {
		return fmt.Errorf("trace: write verification: %w", err)
	}

	if chain.Policy == nil {
		if _, err := fmt.Fprint(out, "      policy: missing\n"); err != nil {
			return fmt.Errorf("trace: write policy: %w", err)
		}
	} else if err := writePolicyHuman(out, chain.Policy); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "      approvals: %d\n", len(chain.Approvals)); err != nil {
		return fmt.Errorf("trace: write approvals: %w", err)
	}
	for j := range chain.Approvals {
		approval := &chain.Approvals[j]
		if _, err := fmt.Fprintf(out,
			"        approval %s decision=%s approver=%s role=%s at=%s\n",
			approval.ID, approval.Decision, approval.Approver.DisplayName,
			approval.Approver.Role, approval.DecidedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("trace: write approval: %w", err)
		}
	}

	if chain.Executor.Audit == nil {
		if _, err := fmt.Fprint(out, "      executor.audit: missing\n"); err != nil {
			return fmt.Errorf("trace: write executor audit: %w", err)
		}
	} else if err := writeExecutorAuditHuman(out, chain.Executor.Audit); err != nil {
		return err
	}

	if chain.LedgerTxn == nil {
		if _, err := fmt.Fprint(out, "      ledger_txn: missing\n"); err != nil {
			return fmt.Errorf("trace: write ledger txn: %w", err)
		}
	} else {
		if _, err := fmt.Fprintf(out,
			"      ledger_txn: %s kind=%s value_date=%s\n",
			chain.LedgerTxn.ID, chain.LedgerTxn.Kind,
			chain.LedgerTxn.ValueDate); err != nil {
			return fmt.Errorf("trace: write ledger txn: %w", err)
		}
		if _, err := fmt.Fprintf(out, "        entries: %d\n", len(chain.LedgerEntries)); err != nil {
			return fmt.Errorf("trace: write entry count: %w", err)
		}
		for j := range chain.LedgerEntries {
			entry := &chain.LedgerEntries[j]
			if _, err := fmt.Fprintf(out,
				"          entry %s direction=%s amount_minor=%d currency=%s account=%s\n",
				entry.ID, entry.Direction, entry.AmountMinor, entry.Currency,
				entry.AccountCode); err != nil {
				return fmt.Errorf("trace: write ledger entry: %w", err)
			}
		}
	}
	return writeChainMissing(out, chain)
}

func writePolicyHuman(out io.Writer, policy *traceability.Policy) error {
	reasons := strings.Join(policy.Reasons, ",")
	if reasons == "" {
		reasons = "none"
	}
	if _, err := fmt.Fprintf(out,
		"      policy: risk_tier=%s rule=%s reasons=%s config_hash=%s\n",
		textOrNone(policy.RiskTier), policy.Rule, reasons,
		policy.ConfigHash); err != nil {
		return fmt.Errorf("trace: write policy: %w", err)
	}
	if policy.ConfigLoadedAt == nil {
		if _, err := fmt.Fprint(out, "        config_loaded_at: missing (policy_config)\n"); err != nil {
			return fmt.Errorf("trace: write policy config: %w", err)
		}
	} else if _, err := fmt.Fprintf(out, "        config_loaded_at: %s\n",
		policy.ConfigLoadedAt.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("trace: write policy config: %w", err)
	}
	if policy.ConfigYAML == nil {
		if _, err := fmt.Fprint(out, "        config_yaml: missing (policy_config)\n"); err != nil {
			return fmt.Errorf("trace: write policy config: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprint(out, "        config_yaml:\n"); err != nil {
		return fmt.Errorf("trace: write policy config: %w", err)
	}
	for _, line := range strings.Split(strings.TrimRight(*policy.ConfigYAML, "\n"), "\n") {
		if _, err := fmt.Fprintf(out, "          %s\n", line); err != nil {
			return fmt.Errorf("trace: write policy config: %w", err)
		}
	}
	return nil
}

// executorAfter is the executor.applied after payload.
type executorAfter struct {
	Status        string `json:"status"`
	LedgerTxnID   string `json:"ledger_txn_id"`
	AppliedAt     string `json:"applied_at"`
	ExecutorRunID string `json:"executor_run_id"`
}

func writeExecutorAuditHuman(out io.Writer, audit *traceability.ExecutorAudit) error {
	var after executorAfter
	if len(audit.After) > 0 && json.Unmarshal(audit.After, &after) == nil {
		if _, err := fmt.Fprintf(out,
			"      executor.audit: seq=%d actor=%s applied_at=%s ledger_txn=%s executor_run=%s\n",
			audit.Seq, audit.Actor, textOrNone(stringPtr(after.AppliedAt)),
			textOrNone(stringPtr(after.LedgerTxnID)),
			textOrNone(stringPtr(after.ExecutorRunID))); err != nil {
			return fmt.Errorf("trace: write executor audit: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(out,
		"      executor.audit: seq=%d actor=%s after=%s\n",
		audit.Seq, audit.Actor, audit.After); err != nil {
		return fmt.Errorf("trace: write executor audit: %w", err)
	}
	return nil
}

func writeChainMissing(out io.Writer, chain *traceability.Chain) error {
	if _, err := fmt.Fprintf(out, "    missing: %s\n", markersOrNone(chain.Missing)); err != nil {
		return fmt.Errorf("trace: write chain missing: %w", err)
	}
	return nil
}

func markersOrNone(markers []string) string {
	if len(markers) == 0 {
		return "none"
	}
	return strings.Join(markers, ", ")
}

func textOrNone(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "none"
	}
	return *value
}

func stringPtr(value string) *string {
	return &value
}

// traceUsageError marks a flag/argument problem as exit 2.
func traceUsageError(format string, args ...any) error {
	return &exitError{code: 2, err: fmt.Errorf("trace: "+format, args...)}
}
