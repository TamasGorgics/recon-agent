package agent

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agent/proposal"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/audit"
	"github.com/TamasGorgics/recon-agent/internal/cases"
	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/db"
	"github.com/TamasGorgics/recon-agent/internal/llm"
)

// AppPool is the app-role pool the loop uses for run persistence, case
// transitions, flags, and the kill switch. It is never given to tools.
type AppPool interface {
	RunDB
	db.Beginner
	tools.Execer
}

// SetupDeps is the production wiring input. Agent is the recon_agent_ro read
// surface; AgentExec is the same pool's INSERT-only proposal surface.
type SetupDeps struct {
	App             AppPool
	Agent           tools.Queryer
	AgentExec       tools.Execer
	Client          llm.Client
	Clock           clock.Clock
	Config          config.Config
	UntrustedFields map[string][]string
	NewRunID        func(caseID string) (string, error)
	BeforeStep      func(ctx context.Context) error
	NewProposalID   func() (string, error)
	Logger          *slog.Logger
}

// NewProductionRunner registers the read and proposal tools, builds the app
// services, and returns the loop runner used by the worker and CLI.
func NewProductionRunner(deps SetupDeps) (*Runner, error) {
	if deps.App == nil || deps.Agent == nil || deps.AgentExec == nil || deps.Client == nil || deps.Clock == nil {
		return nil, fmt.Errorf("%w: app pool, agent queryer, agent exec, client and clock are required", ErrInvalidConfig)
	}
	model := strings.TrimSpace(deps.Config.Agent.Model)
	if model == "" {
		model = config.NormalizeLLMModel(deps.Config.LLM.Model)
	}
	agentConfig := deps.Config.Agent
	agentConfig.Model = model

	registry := tools.NewRegistry()
	readDeps := tools.Deps{
		Agent:           deps.Agent,
		Clock:           deps.Clock,
		UntrustedFields: deps.UntrustedFields,
	}
	if err := tools.RegisterPart1(registry, readDeps); err != nil {
		return nil, fmt.Errorf("agent: register read tools: %w", err)
	}
	if err := tools.RegisterPart2(registry, readDeps); err != nil {
		return nil, fmt.Errorf("agent: register read tools: %w", err)
	}
	proposalDeps := proposal.Deps{
		App:       deps.App,
		AgentExec: deps.AgentExec,
		Clock:     deps.Clock,
		Ledger:    deps.Config.Ledger,
		NewID:     deps.NewProposalID,
	}
	if err := proposal.Register(registry, proposalDeps); err != nil {
		return nil, fmt.Errorf("agent: register proposal tools: %w", err)
	}

	actor, err := audit.AgentActor(model)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	caseService, err := cases.NewService(deps.App, deps.Clock, actor)
	if err != nil {
		return nil, fmt.Errorf("agent: create case service: %w", err)
	}

	return NewRunner(Deps{
		Store:      NewDBStore(deps.App),
		Cases:      caseService,
		Agent:      deps.Agent,
		Client:     deps.Client,
		Registry:   registry,
		Recorder:   tools.NewDBRecorder(deps.App),
		Clock:      deps.Clock,
		Config:     agentConfig,
		Switch:     DBSwitch{Queryer: deps.App},
		FlagsExec:  deps.App,
		NewRunID:   deps.NewRunID,
		BeforeStep: deps.BeforeStep,
		Logger:     deps.Logger,
		Provider:   deps.Config.LLM.Provider,
		BaseURL:    deps.Config.LLM.BaseURL,
	})
}

// newUUIDv4 returns a random RFC 4122 version 4 UUID from crypto/rand.
func newUUIDv4() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("agent: generate uuid: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
