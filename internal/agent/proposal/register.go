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
	proposalTools := []tools.Tool{
		&linkTool{deps: normalized},
		&adjustmentTool{deps: normalized},
		&writeOffTool{deps: normalized},
		&duplicateTool{deps: normalized},
		&requestInfoTool{deps: normalized},
		&escalateTool{},
	}
	for _, tool := range proposalTools {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}
	return nil
}
