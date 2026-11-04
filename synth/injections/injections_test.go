package injections_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/synth/injections"
)

// TestEmbeddedTemplatesAreSortedDataFiles proves the default injection
// templates are loaded from the embedded .txt data files, deterministic and
// non-empty.
func TestEmbeddedTemplatesAreSortedDataFiles(t *testing.T) {
	templates, err := injections.All()
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(templates) < 2 {
		t.Fatalf("embedded templates = %d, want at least 2", len(templates))
	}
	seenNames := make(map[string]struct{}, len(templates))
	seenText := make(map[string]struct{}, len(templates))
	for i, template := range templates {
		if template.Name == "" || !strings.HasSuffix(template.Name, ".txt") {
			t.Errorf("template %d name = %q, want a .txt file", i, template.Name)
		}
		if _, duplicate := seenNames[template.Name]; duplicate {
			t.Errorf("duplicate template name %q", template.Name)
		}
		seenNames[template.Name] = struct{}{}
		if strings.TrimSpace(template.Text) != template.Text || template.Text == "" {
			t.Errorf("template %s text is empty or not trimmed: %q", template.Name, template.Text)
		}
		if strings.ContainsRune(template.Text, '\x00') {
			t.Errorf("template %s contains NUL", template.Name)
		}
		if _, duplicate := seenText[template.Text]; duplicate {
			t.Errorf("template %s duplicates another template's text", template.Name)
		}
		seenText[template.Text] = struct{}{}
		if i > 0 && templates[i-1].Name >= template.Name {
			t.Errorf("templates are not sorted: %q before %q", templates[i-1].Name, template.Name)
		}
	}
}

// TestLoadDir proves templates can be loaded from an arbitrary directory, in
// name order, so M2-07 and M6-02 can reuse a different template set.
func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "b.txt", "Second instruction.")
	writeTemplate(t, dir, "a.txt", "First instruction.")

	templates, err := injections.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(templates) != 2 {
		t.Fatalf("templates = %d, want 2", len(templates))
	}
	if templates[0].Name != "a.txt" || templates[0].Text != "First instruction." {
		t.Errorf("templates[0] = %+v", templates[0])
	}
	if templates[1].Name != "b.txt" {
		t.Errorf("templates[1] = %+v", templates[1])
	}
}

// TestLoadRejectsEmptySet proves a template directory cannot silently yield an
// injection_memo scenario with no template.
func TestLoadRejectsEmptySet(t *testing.T) {
	dir := t.TempDir()
	if _, err := injections.LoadDir(dir); err == nil {
		t.Fatal("LoadDir(empty) = nil error, want error")
	}
	writeTemplate(t, dir, "blank.txt", "   \n")
	if _, err := injections.LoadDir(dir); err == nil {
		t.Fatal("LoadDir(blank) = nil error, want error")
	}
}

func writeTemplate(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
