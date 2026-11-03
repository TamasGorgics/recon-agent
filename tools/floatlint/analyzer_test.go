package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"
)

const modulePath = "github.com/TamasGorgics/recon-agent"

// covers: FIN-01
func TestAnalyzerFixtures(t *testing.T) {
	// The config deliberately omits "unscoped": the same floating-point code
	// must be allowed there, proving the analyzer is package-scoped.
	cfg := Config{Packages: []string{"good", "bad", "allow"}}
	a := New(cfg)
	analysistest.Run(t, analysistest.TestData(), a, "good", "bad", "allow", "unscoped")
}

// covers: FIN-01
func TestAllowRequiresNonEmptyReason(t *testing.T) {
	src := `package fixture

var Allowed float64 //floatlint:allow confidence is a 0..1 score, not money
var Flagged float64 //floatlint:allow
`
	diags := runAnalyzer(t, Config{Packages: []string{"example.com/fixture"}}, src)
	var messages []string
	floatCount := 0
	for _, d := range diags {
		messages = append(messages, d.Message)
		if strings.Contains(d.Message, "float64 is not allowed") {
			floatCount++
		}
	}
	joined := strings.Join(messages, "\n")

	if floatCount != 1 {
		t.Fatalf("got %d float64 diagnostics, want 1 (the valid directive must suppress the other line); messages: %v", floatCount, messages)
	}
	if !strings.Contains(joined, "requires a non-empty reason") {
		t.Fatalf("missing empty-reason diagnostic; got: %v", messages)
	}
}

// covers: FIN-01
func TestAllowSuppressesExactlyOneLine(t *testing.T) {
	src := `package fixture

var Allowed = 1.5 //floatlint:allow a test tolerance, not money
var NotAllowed = 2.5
`
	diags := runAnalyzer(t, Config{Packages: []string{"example.com/fixture"}}, src)
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want exactly 1 for the undirected line: %v", len(diags), diags)
	}
	if !strings.Contains(diags[0].Message, "floating-point literal") {
		t.Fatalf("unexpected diagnostic: %q", diags[0].Message)
	}
}

// covers: FIN-01
func TestParseAllowDirective(t *testing.T) {
	tests := []struct {
		text    string
		reason  string
		present bool
	}{
		{"//floatlint:allow confidence is not money", "confidence is not money", true},
		{"// floatlint:allow latency in milliseconds", "latency in milliseconds", true},
		{"//floatlint:allow", "", true},
		{"//floatlint:allow   ", "", true},
		{"//floatlint:allowed", "", false},
		{"// floatlint:allowing this", "", false},
		{"// other comment", "", false},
		{"/* floatlint:allow nope */", "", false},
	}
	for _, tt := range tests {
		reason, present := parseAllowDirective(tt.text)
		if reason != tt.reason || present != tt.present {
			t.Errorf("parseAllowDirective(%q) = (%q, %v), want (%q, %v)",
				tt.text, reason, present, tt.reason, tt.present)
		}
	}
}

// covers: FIN-01
func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	required := []string{
		"internal/money",
		"internal/ledger",
		"internal/match",
		"internal/cases",
		"internal/policy",
		"internal/approval",
		"internal/executor",
		"internal/audit",
		"internal/agent",
	}
	for _, pkg := range required {
		importPath := modulePath + "/" + pkg
		if !cfg.Applies(importPath) {
			t.Errorf("config does not cover %s", importPath)
		}
	}
	if cfg.Applies(modulePath + "/tools/floatlint") {
		t.Error("config unexpectedly covers the floatlint tool itself")
	}
	if cfg.Applies("github.com/TamasGorgics/recon-agent/internal/moneyfoo") {
		t.Error("config suffix match is not scoped to a path segment")
	}
}

// runAnalyzer loads a single-file module and returns the diagnostics emitted
// by the floatlint analyzer. It is used for assertions that analysistest's
// // want comments cannot express (for example a directive with no reason).
func runAnalyzer(t *testing.T, cfg Config, src string) []analysis.Diagnostic {
	t.Helper()

	dir := t.TempDir()
	gomod := "module example.com/fixture\n\ngo 1.27.3\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	pkgs, err := packages.Load(&packages.Config{Mode: packages.LoadAllSyntax, Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("packages.Load returned %d packages, want 1", len(pkgs))
	}

	graph, err := checker.Analyze([]*analysis.Analyzer{New(cfg)}, pkgs, &checker.Options{Sequential: true})
	if err != nil {
		t.Fatalf("checker.Analyze: %v", err)
	}
	var diags []analysis.Diagnostic
	for act := range graph.All() {
		if act.IsRoot {
			diags = append(diags, act.Diagnostics...)
		}
	}
	return diags
}
