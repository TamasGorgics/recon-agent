package sandbox

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNonTestCodeDoesNotImportTesting enforces the eval/sandbox contract: only
// _test.go files may import the testing package. The package still depends on
// internal/testdb for EnsureTemplate; this check is about direct imports.
func TestNonTestCodeDoesNotImportTesting(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range parsed.Imports {
			if strings.Trim(imported.Path.Value, `"`) == "testing" {
				t.Fatalf("%s imports testing outside a test file", name)
			}
		}
	}
	if files == 0 {
		t.Fatal("found no non-test Go files")
	}
}
