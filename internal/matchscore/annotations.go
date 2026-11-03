package matchscore

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// M1CoverageSubset is the FIN set the M1-16 gate checks. D-8/Q-03 make M1-16 a
// documented subset gate: M1 tests also touch FIN-24, FIN-30/31/33,
// FIN-40/41/42/43, FIN-54 and FIN-56, but full requirement coverage belongs to
// M6-07's checker (`make fin-coverage`). A missing annotation inside this
// subset fails M1-16; annotations outside it are additional evidence.
var m1CoverageSubset = []string{
	"FIN-01", "FIN-02", "FIN-03", "FIN-04", "FIN-05", "FIN-06",
	"FIN-10", "FIN-11", "FIN-12", "FIN-13", "FIN-14",
	"FIN-50", "FIN-51", "FIN-55",
}

// SubsetRationale records why the gate does not require every FIN.
const SubsetRationale = "M1-16 checks FIN-01..06, 10..14, 50, 51 and 55 only. " +
	"The remaining M1-visible requirements (FIN-24, 30/31/33, 40/41/42/43, 54, 56) " +
	"are covered by M1 tests but are outside this subset until the full FIN " +
	"coverage checker lands in M6-07 (Q-03, D-8)."

// M1CoverageSubset returns a copy of the documented subset.
func M1CoverageSubset() []string {
	return append([]string(nil), m1CoverageSubset...)
}

var (
	coverageLine = regexp.MustCompile(`^\s*covers:\s*(.+?)\s*$`)
	finID        = regexp.MustCompile(`FIN-[0-9]{2}`)
)

var skippedDirs = map[string]struct{}{
	".git":         {},
	".team":        {},
	"node_modules": {},
	"tmp":          {},
	"vendor":       {},
}

// Coverage returns the sorted test files that annotate each FIN id found under
// root. Only *_test.go files count; a `// covers: FIN-01, FIN-02` comment may
// list several IDs. Files are parsed with go/parser, so comment-like text
// inside string literals (such as test fixtures) is ignored.
func Coverage(root string) (map[string][]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("matchscore: coverage root is required")
	}
	covered := make(map[string]map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if _, skip := skippedDirs[entry.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("matchscore: read %s: %w", path, err)
		}
		ids, err := finIDsIn(path, data)
		if err != nil {
			return fmt.Errorf("matchscore: parse %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			relative = path
		}
		for _, id := range ids {
			if covered[id] == nil {
				covered[id] = make(map[string]struct{})
			}
			covered[id][filepath.ToSlash(relative)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("matchscore: walk coverage root: %w", err)
	}

	out := make(map[string][]string, len(covered))
	for id, files := range covered {
		list := make([]string, 0, len(files))
		for file := range files {
			list = append(list, file)
		}
		sort.Strings(list)
		out[id] = list
	}
	return out, nil
}

// finIDsIn returns the FIN ids annotated by `// covers:` comments in one Go
// file. It parses the file so comment-like text inside string literals (for
// example the scanner's own raw-string fixtures) can never satisfy the gate.
func finIDsIn(filename string, contents []byte) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, contents, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var ids []string
	for _, group := range parsed.Comments {
		for _, comment := range group.List {
			if !strings.HasPrefix(comment.Text, "//") {
				continue
			}
			body := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			match := coverageLine.FindStringSubmatch(body)
			if match == nil {
				continue
			}
			for _, id := range finID.FindAllString(match[1], -1) {
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// MissingCoverage returns the required FIN ids that have no annotated test
// under root, in the caller's required order. An empty result means the gate
// passes.
func MissingCoverage(root string, required []string) ([]string, error) {
	covered, err := Coverage(root)
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0)
	for _, id := range required {
		if len(covered[id]) == 0 {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
