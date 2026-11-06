package agent

import (
	"fmt"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// loadPrompt validates and loads a released prompt version before any database
// or model work.
func loadPrompt(version string) (prompts.Prompt, error) {
	return prompts.Load(version)
}

// toolDefinitions returns the sorted provider-neutral tool definitions. The
// registry already returns names sorted, and every schema is a closed object.
func toolDefinitions(registry *tools.Registry) ([]llm.ToolDef, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: nil tool registry", ErrInvalidConfig)
	}
	names := registry.Names()
	definitions := make([]llm.ToolDef, 0, len(names))
	for _, name := range names {
		tool, ok := registry.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("agent: registry lost tool %q", name)
		}
		definitions = append(definitions, llm.ToolDef{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.InputSchema(),
		})
	}
	return definitions, nil
}
