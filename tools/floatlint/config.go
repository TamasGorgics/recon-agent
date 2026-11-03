package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config lists the packages where floating-point use is forbidden.
//
// Each entry is an import path or a module-relative path. A package matches
// when its import path equals the entry, or ends with "/<entry>". The latter
// keeps the checked-in config readable ("internal/money") without hard-coding
// the module path in the analyzer.
type Config struct {
	Packages []string `yaml:"packages"`
}

// LoadConfig reads and validates a floatlint configuration file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.Packages) == 0 {
		return Config{}, fmt.Errorf("config %s lists no packages", path)
	}
	for i, pkg := range cfg.Packages {
		if strings.Trim(strings.TrimSpace(pkg), "/") == "" {
			return Config{}, fmt.Errorf("config %s has an empty package entry at index %d", path, i)
		}
	}
	return cfg, nil
}

// Applies reports whether importPath is one of the configured packages.
func (c Config) Applies(importPath string) bool {
	for _, entry := range c.Packages {
		entry = strings.Trim(strings.TrimSpace(entry), "/")
		if entry == "" {
			continue
		}
		if importPath == entry || strings.HasSuffix(importPath, "/"+entry) {
			return true
		}
	}
	return false
}
