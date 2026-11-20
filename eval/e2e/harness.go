package e2e

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/TamasGorgics/recon-agent/eval/approver"
	"github.com/TamasGorgics/recon-agent/internal/approval"
	proposalstate "github.com/TamasGorgics/recon-agent/internal/approval/state"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/executor"
	"github.com/TamasGorgics/recon-agent/internal/ledger"
	"github.com/TamasGorgics/recon-agent/internal/limits"
	"github.com/TamasGorgics/recon-agent/internal/policy"
	"github.com/TamasGorgics/recon-agent/internal/policy/verify"
)

// DefaultPolicyPath is the committed policy document. Callers running from a
// package directory pass an explicit path.
const DefaultPolicyPath = config.DefaultPolicyFile

// DefaultExecutorRunID is the stable executor identity used when the caller
// does not supply one. The runner should pass its own stable run id.
const DefaultExecutorRunID = "executor:eval-e2e"

// HarnessOptions wires the real services for one sandbox case. Admin is used
// only to seed the synthetic approver; reads and approvals use App; the
// executor writes use Exec.
type HarnessOptions struct {
	// Admin is the sandbox maintenance pool (INSERT approvers).
	Admin *pgxpool.Pool
	// App is the recon_app pool (reads, approval, verifier, limits).
	App *pgxpool.Pool
	// Exec is the recon_executor pool (executor apply, ledger writes).
	Exec db.Beginner
	// Clock is the case's fake clock. It is required for deterministic
	// expires_at/decided_at/applied_at and is never replaced by a wall clock.
	Clock clock.Clock
	// PolicyPath defaults to policy.yaml.
	PolicyPath string
	// ExecutorRunID defaults to DefaultExecutorRunID.
	ExecutorRunID string
	// LedgerConfig is the adjustment account allowlist the eval world was
	// loaded with. The zero value uses config.DefaultLedgerConfig. The runner
	// passes the sandbox dataset's ledger config so the executor accepts the
	// generated account codes.
	LedgerConfig config.LedgerConfig
	// Verifier overrides the production verifier. Nil uses the real M4-05
	// service; tests that need a deterministic T0 pattern inject a stub.
	Verifier approval.Verifier
	// TTL overrides the policy document's proposal TTL when positive.
	TTL time.Duration
}

// NewHarness builds a case-scoped Pipeline with production service wiring. It
// seeds the synthetic approver through Admin before constructing the approver,
// and it never seeds through App (recon_app has no INSERT grant on approvers).
func NewHarness(ctx context.Context, opts HarnessOptions) (*Pipeline, error) {
	if ctx == nil {
		return nil, errors.New("eval/e2e: harness context is required")
	}
	if opts.Admin == nil {
		return nil, errors.New("eval/e2e: admin pool is required")
	}
	if opts.App == nil {
		return nil, errors.New("eval/e2e: app pool is required")
	}
	if opts.Exec == nil {
		return nil, errors.New("eval/e2e: executor pool is required")
	}
	if opts.Clock == nil {
		return nil, errors.New("eval/e2e: clock is required")
	}
	policyPath := opts.PolicyPath
	if policyPath == "" {
		policyPath = DefaultPolicyPath
	}
	executorRunID := opts.ExecutorRunID
	if executorRunID == "" {
		executorRunID = DefaultExecutorRunID
	}
	ledgerConfig := opts.LedgerConfig
	if len(ledgerConfig.AdjustmentAccounts) == 0 {
		ledgerConfig = config.DefaultLedgerConfig()
	}

	if err := approver.EnsureSyntheticApprover(ctx, opts.Admin, approver.SyntheticApproverID); err != nil {
		return nil, err
	}

	loadedPolicy, err := policy.LoadFile(policyPath)
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: load policy %q: %w", policyPath, err)
	}
	engine, err := policy.NewEngine(loadedPolicy, policy.Options{})
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build policy engine: %w", err)
	}
	verifier := opts.Verifier
	if verifier == nil {
		verifierService, err := verify.NewService(opts.App, verify.OptionsFromConfig(config.VerifierConfig{}))
		if err != nil {
			return nil, fmt.Errorf("eval/e2e: build verifier: %w", err)
		}
		verifier = approval.VerifierAdapter{Service: verifierService}
	}
	limitsService, err := limits.NewService(limits.Options{
		Pool:   opts.App,
		Clock:  opts.Clock,
		Config: config.DefaultAutoApplyLimitsConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build limits service: %w", err)
	}
	approvalState, err := proposalstate.NewService(opts.App, opts.Clock)
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build approval state service: %w", err)
	}
	approvalService, err := approval.NewService(approval.Options{
		Pool:     opts.App,
		Clock:    opts.Clock,
		State:    approvalState,
		Verifier: verifier,
		Limits:   limitsService,
		Policy:   approval.Policy{Engine: engine, Loaded: loadedPolicy},
		TTL:      opts.TTL,
	})
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build approval service: %w", err)
	}
	executorState, err := proposalstate.NewService(opts.Exec, opts.Clock)
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build executor state service: %w", err)
	}
	executorService, err := executor.NewService(executor.Options{
		Pool:          opts.Exec,
		Ledger:        ledger.NewService(opts.Clock, ledgerConfig),
		State:         executorState,
		Clock:         opts.Clock,
		ExecutorRunID: executorRunID,
	})
	if err != nil {
		return nil, fmt.Errorf("eval/e2e: build executor service: %w", err)
	}
	readModel, err := NewReadModel(opts.App)
	if err != nil {
		return nil, err
	}
	simulated, err := approver.New(approvalService, readModel, approver.Config{
		ApproverID: approver.SyntheticApproverID,
		Source:     approver.SyntheticSource,
	})
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		Approval: approvalService,
		Executor: executorService,
		Approver: simulated,
		Tieout:   opts.App,
		Loader:   readModel,
		Clock:    opts.Clock,
	}, nil
}
