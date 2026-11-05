package tools

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Registry holds the explicitly registered tools and their compiled schemas.
// It performs no implicit catalog registration; callers compose RegisterPart1
// and RegisterPart2 themselves.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*registeredTool
}

type registeredTool struct {
	tool   Tool
	schema *jsonschema.Schema
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*registeredTool)}
}

// Register validates and compiles a tool definition. It rejects nil tools,
// empty or duplicate names, malformed or uncompilable schemas, non-object
// top-level schemas, and any object schema that does not set
// additionalProperties to false.
func (r *Registry) Register(tool Tool) error {
	if isNilTool(tool) {
		return fmt.Errorf("%w: nil", ErrInvalidTool)
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidTool)
	}

	r.mu.RLock()
	_, exists := r.tools[name]
	r.mu.RUnlock()
	if exists {
		return fmt.Errorf("%w: %q", ErrDuplicateTool, name)
	}

	raw := tool.InputSchema()
	if len(raw) == 0 {
		return fmt.Errorf("%w: %q has an empty schema", ErrInvalidSchema, name)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidSchema, name, err)
	}
	if err := requireClosedObjectSchema(document); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidSchema, name, err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	location := "https://recon.local/schemas/tools/" + url.PathEscape(name) + ".json"
	if err := compiler.AddResource(location, document); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidSchema, name, err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidSchema, name, err)
	}

	entry := &registeredTool{tool: tool, schema: schema}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateTool, name)
	}
	r.tools[name] = entry
	return nil
}

// Lookup returns a registered tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	entry, ok := r.lookup(name)
	if !ok {
		return nil, false
	}
	return entry.tool, true
}

// Names returns the registered tool names sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) lookup(name string) (*registeredTool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	return entry, ok
}

func isNilTool(tool Tool) bool {
	if tool == nil {
		return true
	}
	value := reflect.ValueOf(tool)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// requireClosedObjectSchema enforces the tool-schema contract: an object at
// the root and additionalProperties=false on every object schema, not only the
// top level.
func requireClosedObjectSchema(document any) error {
	root, ok := document.(map[string]any)
	if !ok {
		return fmt.Errorf("top-level schema must be an object")
	}
	if !schemaIsObject(root) {
		return fmt.Errorf("top-level schema must declare type=object")
	}
	return inspectClosedObjectSchemas(root)
}

func inspectClosedObjectSchemas(node any) error {
	switch value := node.(type) {
	case map[string]any:
		if schemaIsObject(value) {
			closed, ok := value["additionalProperties"].(bool)
			if !ok || closed {
				property := ""
				if props, ok := value["properties"].(map[string]any); ok {
					for name := range props {
						property = name
						break
					}
				}
				if property != "" {
					return fmt.Errorf("object schema for property %q must set additionalProperties=false", property)
				}
				return fmt.Errorf("every object schema must set additionalProperties=false")
			}
		}
		for _, child := range value {
			if err := inspectClosedObjectSchemas(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := inspectClosedObjectSchemas(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaIsObject(schema map[string]any) bool {
	if _, ok := schema["properties"]; ok {
		return true
	}
	if _, ok := schema["required"]; ok {
		return true
	}
	switch value := schema["type"].(type) {
	case string:
		return value == "object"
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && text == "object" {
				return true
			}
		}
	}
	return false
}
