package e2e

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestNoTruthOrNetworkImports is the package-boundary proof. The non-test
// sources read only operational tables and never import an answer source or a
// network/process package.
func TestNoTruthOrNetworkImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	forbiddenImportSuffixes := []string{
		"/eval/dataset",
		"/eval/score",
		"/eval/truth",
		"/eval/runner",
		"/eval/gate",
		"/eval/report",
	}
	forbiddenText := []string{
		"FROM eval_truth",
		"eval_truth.scenarios",
		"eval_truth.scenario_cases",
		"eval_truth.sandbox_meta",
		"expected_resolutions",
		"primary_resolution",
		"break_signature",
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			for _, suffix := range forbiddenImportSuffixes {
				if strings.HasSuffix(path, suffix) || strings.Contains(path, suffix+"/") {
					t.Errorf("%s imports forbidden answer-source package %q", name, path)
				}
			}
			if path == "net" || strings.HasPrefix(path, "net/") || path == "os/exec" {
				t.Errorf("%s imports network/process package %q", name, path)
			}
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, text := range forbiddenText {
			if strings.Contains(string(source), text) {
				t.Errorf("%s contains forbidden answer-source text %q", name, text)
			}
		}
	}
}
