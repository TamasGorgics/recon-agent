package state_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// caseStatusWriterPattern finds a SQL statement that assigns cases.status. It
// deliberately matches only `SET status =`, not predicate comparisons such as
// `AND status = 'in_agent'`: after wave 1b-i the lease paths still compare
// status inside non-status updates, and those are not bypasses. The 600-byte
// window covers the current multi-line statements; the structure test is a
// guard against new bypasses, not a SQL parser.
var caseStatusWriterPattern = regexp.MustCompile(`(?is)UPDATE\s+cases\b.{0,600}?\bSET\s+status\s*=`)

// authorityWriter is the one production file allowed to assign cases.status.
// Every other production writer is a regression: the authority owns the edge,
// actor, reason, resolution, lease, and audit rules, so a direct UPDATE would
// bypass them. Test fixtures are excluded by the _test.go skip below.
const authorityWriter = "internal/cases/state/state.go"

// TestNoUndocumentedCaseStatusWriter scans production Go files for direct
// cases.status assignments. Only internal/cases/state may assign status; any
// other writer, inside or outside internal/cases, fails the test. There is no
// allowlist to extend.
func TestNoUndocumentedCaseStatusWriter(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	var writers []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || name == "vendor" || name == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		normalized := stripLineComments(string(raw))
		if !caseStatusWriterPattern.MatchString(normalized) {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		writers = append(writers, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("scan repository: %v", err)
	}
	sort.Strings(writers)

	var undocumented []string
	for _, writer := range writers {
		if writer != authorityWriter {
			undocumented = append(undocumented, writer)
		}
	}
	if len(undocumented) > 0 {
		t.Fatalf("cases.status assigned outside the state authority: %v (move it to internal/cases/state); all writers: %v",
			undocumented, writers)
	}
	if !contains(writers, authorityWriter) {
		t.Fatalf("state authority not detected as a cases.status writer; writers: %v", writers)
	}
	if len(writers) != 1 {
		t.Fatalf("expected exactly one cases.status writer, got %v", writers)
	}
	t.Logf("sole cases.status writer: %s", authorityWriter)
}

func stripLineComments(source string) string {
	lines := strings.Split(source, "\n")
	out := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
