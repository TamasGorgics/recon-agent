package proposal

import (
	"errors"
	"fmt"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// Register registers the five proposal tools and escalate on registry.
// Registration is explicit; M2-09 composes it with the read tools. A nil
// registry, missing AgentExec, missing evidence source or duplicate tool name
// is an error.
func Register(registry *tools.Registry, deps Deps) error {
	if registry == nil {
		return fmt.Errorf("%w: nil registry", tools.ErrInvalidTool)
	}
	if deps.AgentExec == nil {
		return errors.New("proposal: Register: AgentExec is required")
	}
	normalized, err := normalizeDeps(deps)
	if err != nil {
		return err
	}
	return registerTools(registry, normalized)
}

// RegisterDefinitions registers the proposal tools with no dependency wiring.
// It exists so request-hash reconstruction can build the exact production tool
// definition list without a database handle. The registered tools must never be
// called: they are definitions only.
func RegisterDefinitions(registry *tools.Registry) error {
	if registry == nil {
		return fmt.Errorf("%w: nil registry", tools.ErrInvalidTool)
	}
	return registerTools(registry, Deps{})
}

func registerTools(registry *tools.Registry, deps Deps) error {
	proposalTools := []tools.Tool{
		&linkTool{deps: deps},
		&adjustmentTool{deps: deps},
		&writeOffTool{deps: deps},
		&duplicateTool{deps: deps},
		&requestInfoTool{deps: deps},
		&escalateTool{},
	}
	for _, tool := range proposalTools {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}
