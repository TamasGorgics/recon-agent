// Package models loads the thin, top-level eval model registry (models.yaml).
//
// models.yaml is eval-facing metadata, not a second source of truth for
// capabilities or prices. It names the primary and second model and their
// fallback policy; every claim is checked against the embedded
// internal/llm/pricing.yaml registry. Prices and capability tables belong only
// in pricing.yaml.
package models

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

const (
	// SchemaVersion is the only supported models.yaml schema.
	SchemaVersion = 1
	// SourceName is the canonical (and only) pricing source.
	SourceName = "internal/llm/pricing.yaml"
	// FileName is the repository-root registry file.
	FileName = "models.yaml"

	// RolePrimary is the model used for the primary eval comparisons.
	RolePrimary = "primary"
	// RoleSecond is the second, explicitly selected model. It is never an
	// automatic fallback.
	RoleSecond = "second"
)

// Document is a parsed models.yaml.
type Document struct {
	SchemaVersion int     `yaml:"schema_version"`
	Source        string  `yaml:"source"`
	Models        []Model `yaml:"models"`
	Eval          Eval    `yaml:"eval"`
}

// Model is one eval-facing model entry. It deliberately carries no prices or
// capability table: those are validated against llm.DefaultRegistry().
type Model struct {
	ID              string `yaml:"id"`
	Role            string `yaml:"role"`
	AllowedFallback bool   `yaml:"allowed_fallback"`
}

// Eval holds the eval defaults.
type Eval struct {
	DefaultModel  string `yaml:"default_model"`
	DefaultPrompt string `yaml:"default_prompt"`
}

// Path resolves models.yaml from the module root. It walks up from the current
// working directory to the nearest go.mod, so tests in any package can load
// the committed file without a relative-path guess.
func Path() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("models: working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, FileName), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("models: stat go.mod in %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("models: no go.mod found above the working directory")
		}
		dir = parent
	}
}

// Load parses path with strict fields. Unknown keys, duplicate keys, and any
// attempt to copy prices or capabilities into models.yaml fail here.
func Load(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("models: read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse parses and strictly decodes one models.yaml document. It validates the
// shape but not registry drift; call Validate for that.
func Parse(data []byte) (Document, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Document{}, errors.New("models: document is empty")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("models: parse %s: %w", FileName, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Document{}, fmt.Errorf("models: %s must contain exactly one document", FileName)
	}
	return doc, nil
}

// LoadAndValidate loads path and validates it against registry.
func LoadAndValidate(path string, registry *llm.Registry) (Document, error) {
	doc, err := Load(path)
	if err != nil {
		return Document{}, err
	}
	if err := doc.Validate(registry); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// LoadDefault loads and validates the repository-root models.yaml against the
// embedded pricing registry.
func LoadDefault() (Document, error) {
	path, err := Path()
	if err != nil {
		return Document{}, err
	}
	return LoadAndValidate(path, llm.DefaultRegistry())
}

// Validate checks every models.yaml claim against the pricing registry.
func (d Document) Validate(registry *llm.Registry) error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("models: unsupported schema_version %d, want %d", d.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(d.Source) != SourceName {
		return fmt.Errorf("models: source = %q, want %q", d.Source, SourceName)
	}
	if registry == nil {
		return errors.New("models: pricing registry is required")
	}
	if len(d.Models) == 0 {
		return errors.New("models: models is empty")
	}

	seen := make(map[string]struct{}, len(d.Models))
	roles := make(map[string]string, 2)
	for index, entry := range d.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			return fmt.Errorf("models: models[%d].id is empty", index)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("models: model %q is listed more than once", id)
		}
		seen[id] = struct{}{}

		role := strings.TrimSpace(entry.Role)
		switch role {
		case RolePrimary, RoleSecond:
		default:
			return fmt.Errorf("models: model %q role = %q, want %q or %q", id, entry.Role, RolePrimary, RoleSecond)
		}
		if other, duplicate := roles[role]; duplicate {
			return fmt.Errorf("models: role %q appears for both %q and %q; want exactly one", role, other, id)
		}
		roles[role] = id

		priced, err := registry.Get(id)
		if err != nil {
			return fmt.Errorf("models: model %q is not in %s (pricing drift: add, rename, or remove it in both files)",
				id, SourceName)
		}
		if entry.AllowedFallback != priced.AllowedFallback {
			return fmt.Errorf("models: model %q allowed_fallback = %t, but %s says %t",
				id, entry.AllowedFallback, SourceName, priced.AllowedFallback)
		}
		if !priced.Capabilities.Tools {
			return fmt.Errorf("models: model %q does not advertise tool support in %s; the eval agent requires tools",
				id, SourceName)
		}
	}
	if _, ok := roles[RolePrimary]; !ok {
		return fmt.Errorf("models: exactly one %q model is required", RolePrimary)
	}
	if _, ok := roles[RoleSecond]; !ok {
		return fmt.Errorf("models: exactly one %q model is required", RoleSecond)
	}

	defaultModel := strings.TrimSpace(d.Eval.DefaultModel)
	if defaultModel == "" {
		return errors.New("models: eval.default_model is required")
	}
	if _, listed := seen[defaultModel]; !listed {
		return fmt.Errorf("models: eval.default_model %q is not listed in models", defaultModel)
	}
	defaultPrompt := strings.TrimSpace(d.Eval.DefaultPrompt)
	if defaultPrompt == "" {
		return errors.New("models: eval.default_prompt is required")
	}
	if _, err := prompts.Load(defaultPrompt); err != nil {
		return fmt.Errorf("models: eval.default_prompt %q does not load: %w", defaultPrompt, err)
	}
	return nil
}

// ByRole returns the model id for role.
func (d Document) ByRole(role string) (string, bool) {
	for _, entry := range d.Models {
		if entry.Role == role {
			return entry.ID, true
		}
	}
	return "", false
}
