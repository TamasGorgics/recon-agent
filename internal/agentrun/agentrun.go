// Package agentrun is the command-layer helper shared by `recon agent run` and
// the M2 gate. It leases exactly one named case, builds the production runner
// with either cassette replay or a live client, runs the bounded loop, and
// applies the worker's outcome policy.
package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/internal/agent"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/internal/worker"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// Mode is the locked --llm selector. Scripted is cassette replay (no network,
// no key); opencode is the live resilient client.
type Mode string

const (
	ModeScripted Mode = "scripted"
	ModeOpencode Mode = "opencode"
)

// DefaultLeaseTTL matches the worker lease TTL.
const DefaultLeaseTTL = 3 * time.Minute

// Options are the command flags after parsing.
type Options struct {
	CaseID        string
	Mode          Mode
	PromptVersion string
	Model         string
	CassetteDir   string
}

// Deps are the process-boundary dependencies. Client and Cases are optional
// injection points for tests, recording, and the gate; production CLI leaves
// them nil and lets Run build the same stack the worker uses.
type Deps struct {
	Config    config.Config
	App       agent.AppPool
	Agent     tools.Queryer
	AgentExec tools.Execer
	Clock     clock.Clock
	Logger    *slog.Logger
	Client    llm.Client
	Cases     *cases.Service
	NewRunID  func(caseID string) (string, error)
	WorkerID  string
	LeaseTTL  time.Duration
}

// Step is one recorded tool call, safe to print: it never carries arguments or
// results.
type Step struct {
	Seq        int    `json:"seq"`
	Tool       string `json:"tool"`
	ErrorCode  string `json:"error_code,omitempty"`
	DurationMS int    `json:"duration_ms"`
}

// Report is the command result. It contains no raw tool data or memos.
type Report struct {
	CaseID           string          `json:"case_id"`
	RunID            string          `json:"run_id"`
	Outcome          string          `json:"outcome"`
	ProposalID       string          `json:"proposal_id,omitempty"`
	ProposalType     string          `json:"proposal_type,omitempty"`
	ProposalStatus   string          `json:"proposal_status,omitempty"`
	EscalationReason string          `json:"escalation_reason,omitempty"`
	Steps            int             `json:"steps"`
	ToolCalls        int             `json:"tool_calls"`
	TokensIn         int64           `json:"tokens_in"`
	TokensOut        int64           `json:"tokens_out"`
	CostUSD          decimal.Decimal `json:"cost_usd"`
	RepairUsed       bool            `json:"repair_used"`
	PromptVersion    string          `json:"prompt_version"`
	Assistant        string          `json:"assistant,omitempty"`
	ToolSteps        []Step          `json:"tool_steps,omitempty"`
}

// Human writes the human transcript. It prints only sanitized assistant text
// and tool metadata, never raw arguments/results or full memo text.
func (r Report) Human(w io.Writer) error {
	if w == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "case: %s\nrun: %s\nprompt: %s\n", r.CaseID, r.RunID, r.PromptVersion); err != nil {
		return err
	}
	for _, step := range r.ToolSteps {
		status := "ok"
		if step.ErrorCode != "" {
			status = step.ErrorCode
		}
		if _, err := fmt.Fprintf(w, "step %d: %s %s %s\n",
			step.Seq, step.Tool, status, time.Duration(step.DurationMS)*time.Millisecond); err != nil {
			return err
		}
	}
	if r.Assistant != "" {
		if _, err := fmt.Fprintf(w, "assistant: %s\n", r.Assistant); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w,
		"outcome: %s\nsteps: %d tool_calls: %d tokens: %d/%d cost: %s\n",
		r.Outcome, r.Steps, r.ToolCalls, r.TokensIn, r.TokensOut, r.CostUSD.String()); err != nil {
		return err
	}
	if r.ProposalID != "" {
		if _, err := fmt.Fprintf(w, "proposal: %s type=%s status=%s\n",
			r.ProposalID, r.ProposalType, r.ProposalStatus); err != nil {
			return err
		}
	}
	if r.EscalationReason != "" {
		if _, err := fmt.Fprintf(w, "escalation: %s\n", r.EscalationReason); err != nil {
			return err
		}
	}
	return nil
}

// JSON writes one compact JSON object.
func (r Report) JSON(w io.Writer) error {
	if w == nil {
		return nil
	}
	encoder := json.NewEncoder(w)
	return encoder.Encode(r)
}

// Run executes one CLI/gate case. It validates flags, loads the prompt, builds
// the client, leases the named case, runs, and finalizes before returning.
// Usage/config errors happen before any database work.
func Run(ctx context.Context, opts Options, deps Deps) (Report, error) {
	if err := validateOptions(&opts, deps.Config); err != nil {
		return Report{}, err
	}
	if _, err := prompts.Load(opts.PromptVersion); err != nil {
		return Report{}, err
	}
	if deps.App == nil || deps.Agent == nil || deps.AgentExec == nil {
		return Report{}, fmt.Errorf("%w: app pool, agent queryer and agent exec are required", agent.ErrInvalidConfig)
	}
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = strings.TrimSpace(deps.Config.Agent.Model)
	}
	if model == "" {
		model = config.NormalizeLLMModel(deps.Config.LLM.Model)
	}
	model = config.NormalizeLLMModel(model)
	if model == "" {
		return Report{}, errors.New("agent: model is required")
	}

	client := deps.Client
	if client == nil {
		var err error
		client, err = clientForMode(opts, deps, model)
		if err != nil {
			return Report{}, err
		}
	}

	agentConfig := deps.Config.Agent
	agentConfig.Model = model
	agentConfig.PromptVersion = opts.PromptVersion
	if err := agentConfig.Validate(); err != nil {
		return Report{}, fmt.Errorf("agent: config: %w", err)
	}

	workerID := strings.TrimSpace(deps.WorkerID)
	if workerID == "" {
		workerID = DefaultWorkerID()
	}
	leaseTTL := deps.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	newRunID := deps.NewRunID
	if newRunID == nil {
		if opts.Mode == ModeScripted {
			newRunID = agent.DeterministicRunID
		}
	}

	runnerConfig := deps.Config
	runnerConfig.Agent = agentConfig
	runner, err := agent.NewProductionRunner(agent.SetupDeps{
		App:             deps.App,
		Agent:           deps.Agent,
		AgentExec:       deps.AgentExec,
		Client:          client,
		Clock:           deps.Clock,
		Config:          runnerConfig,
		UntrustedFields: runnerConfig.ConfiguredUntrustedFields(),
		NewRunID:        newRunID,
		Logger:          deps.Logger,
	})
	if err != nil {
		return Report{}, err
	}

	caseService := deps.Cases
	if caseService == nil {
		actor, actorErr := audit.AgentActor(model)
		if actorErr != nil {
			return Report{}, fmt.Errorf("%w: %v", agent.ErrInvalidConfig, actorErr)
		}
		caseService, err = cases.NewService(deps.App, deps.Clock, actor)
		if err != nil {
			return Report{}, err
		}
	}

	if _, err := caseService.LeaseByID(ctx, opts.CaseID, workerID, leaseTTL); err != nil {
		return Report{}, fmt.Errorf("agent: lease case %s: %w", opts.CaseID, err)
	}

	result, runErr := runner.Run(ctx, opts.CaseID)
	worker.Finalize(ctx, caseService, workerID, runnerConfig.Worker.MaxAttempts, deps.Logger, opts.CaseID, result, runErr)

	report := Report{
		CaseID:        opts.CaseID,
		RunID:         result.RunID,
		Outcome:       string(result.Outcome),
		ProposalID:    result.ProposalID,
		ProposalType:  result.ProposalType,
		Steps:         result.Steps,
		ToolCalls:     result.ToolCalls,
		TokensIn:      result.TokensIn,
		TokensOut:     result.TokensOut,
		CostUSD:       result.CostUSD,
		RepairUsed:    result.RepairUsed,
		PromptVersion: opts.PromptVersion,
	}
	if report.CostUSD.IsZero() {
		report.CostUSD = decimal.Zero
	}
	if result.Escalation != nil {
		report.EscalationReason = result.Escalation.Reason
	}
	if fillErr := fillReport(ctx, deps.App, &report); fillErr != nil && runErr == nil {
		return report, fillErr
	}

	if runErr != nil {
		// ErrAgentDisabled is a persisted terminal outcome: the loop finalized
		// the run escalated and Finalize released the lease. The CLI reports it
		// like any other terminal outcome and exits 0; only unpersisted/CLI
		// failures below keep exit status 1.
		if errors.Is(runErr, agent.ErrAgentDisabled) {
			return report, nil
		}
		if errors.Is(runErr, llm.ErrCassetteMiss) {
			return report, fmt.Errorf("cassette miss for %s: re-record eval/cassettes after any prompt, tool, or fixture change: %w", opts.CaseID, runErr)
		}
		if opts.Mode == ModeScripted && isRunIDConflict(runErr) {
			return report, fmt.Errorf("scripted run id %s already exists; use a fresh/reset fixture or `make demo-m2`: %w", result.RunID, runErr)
		}
		return report, runErr
	}
	return report, nil
}

func validateOptions(opts *Options, cfg config.Config) error {
	opts.CaseID = strings.ToLower(strings.TrimSpace(opts.CaseID))
	if !tools.IsUUID(opts.CaseID) {
		return fmt.Errorf("agent: --case must be a UUID, got %q", opts.CaseID)
	}
	switch opts.Mode {
	case ModeScripted, ModeOpencode:
	default:
		return fmt.Errorf("agent: --llm must be scripted or opencode, got %q", opts.Mode)
	}
	if strings.TrimSpace(opts.PromptVersion) == "" {
		opts.PromptVersion = cfg.Agent.PromptVersion
	}
	if strings.TrimSpace(opts.PromptVersion) == "" {
		opts.PromptVersion = config.DefaultAgentPromptVersion
	}
	opts.PromptVersion = strings.TrimSpace(opts.PromptVersion)
	if opts.Mode == ModeOpencode && strings.TrimSpace(cfg.LLM.APIKey) == "" {
		return errors.New("agent: --llm opencode requires OPENCODE_API_KEY in the process environment")
	}
	return nil
}

func clientForMode(opts Options, deps Deps, model string) (llm.Client, error) {
	cfg := deps.Config
	cfg.Agent.Model = model
	switch opts.Mode {
	case ModeScripted:
		dir := strings.TrimSpace(opts.CassetteDir)
		if dir == "" {
			dir = llm.DefaultCassetteDir
		}
		return llm.NewCassette(nil, llm.CassetteOptions{
			Mode:     llm.CassetteReplay,
			Dir:      dir,
			Provider: cfg.LLM.Provider,
			BaseURL:  cfg.LLM.BaseURL,
		})
	case ModeOpencode:
		return llm.NewClient(cfg.LLM, llm.Options{Clock: deps.Clock, Logger: deps.Logger})
	default:
		return nil, fmt.Errorf("agent: --llm must be scripted or opencode, got %q", opts.Mode)
	}
}

// DefaultWorkerID is the locked CLI lease owner.
func DefaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("cli:%s:%d", host, os.Getpid())
}

func isRunIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == "agent_runs_pkey"
}
