// Package injections loads the adversarial memo templates used by the
// injection_memo failure mode (spec section 10.1).
//
// Templates are data files, not Go string literals: the embedded set lives in
// synth/injections/*.txt, so M2-07 (untrusted data handling) and M6-02 (eval
// datasets) can reuse or replace them without editing mode logic. The memo is
// evidence only; ground truth is computed from the scenario construction and
// never from memo text.
package injections

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// ErrNoTemplates reports an empty or all-blank template set. An
// injection_memo scenario without a template would not exercise untrusted
// input, so it is a configuration error rather than an empty memo.
var ErrNoTemplates = errors.New("synth/injections: no templates")

// Template is one adversarial memo template loaded from a .txt file. Name is
// the file name; Text is the trimmed file content.
type Template struct {
	Name string
	Text string
}

//go:embed *.txt
var embedded embed.FS

// All returns the embedded templates in ascending file-name order. Each call
// returns a fresh slice; callers may modify their copy.
func All() ([]Template, error) {
	return Load(embedded)
}

// LoadDir loads every *.txt file directly under dir in ascending file-name
// order. It is intended for tests and for eval datasets that supply their own
// template set.
func LoadDir(dir string) ([]Template, error) {
	return Load(os.DirFS(dir))
}

// Load loads every *.txt file at the root of fsys. The order is deterministic;
// blank files, duplicate texts, and an empty set are rejected.
func Load(fsys fs.FS) ([]Template, error) {
	names, err := fs.Glob(fsys, "*.txt")
	if err != nil {
		return nil, fmt.Errorf("synth/injections: glob templates: %w", err)
	}
	sort.Strings(names)
	templates := make([]Template, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("synth/injections: read %s: %w", name, err)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return nil, fmt.Errorf("%w: %s is blank", ErrNoTemplates, name)
		}
		if strings.ContainsRune(text, '\x00') {
			return nil, fmt.Errorf("synth/injections: %s contains NUL", name)
		}
		if previous, duplicate := seen[text]; duplicate {
			return nil, fmt.Errorf("synth/injections: %s duplicates %s", name, previous)
		}
		seen[text] = name
		templates = append(templates, Template{Name: path.Base(name), Text: text})
	}
	if len(templates) == 0 {
		return nil, ErrNoTemplates
	}
	return templates, nil
}
