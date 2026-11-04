package matchscore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TamasGorgics/recon-agent/internal/matchscore"
)

// TestMissingCoverageFindsUnannotatedFINs pins the annotation scanner: a
// required FIN with no // covers: annotation is reported.
func TestMissingCoverageFindsUnannotatedFINs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "internal/example/a_test.go", `package example

// covers: FIN-01, FIN-02
func TestA(t *testing.T) {}
`)
	writeTestFile(t, root, "internal/example/b_test.go", `package example

//covers:FIN-03
func TestB(t *testing.T) {}
`)
	missing, err := matchscore.MissingCoverage(root, []string{"FIN-01", "FIN-02", "FIN-03", "FIN-04"})
	if err != nil {
		t.Fatalf("MissingCoverage: %v", err)
	}
	if !reflect.DeepEqual(missing, []string{"FIN-04"}) {
		t.Fatalf("missing = %v, want [FIN-04]", missing)
	}
}

// TestMissingCoverageIgnoresNonTestFilesAndNonCoverComments pins the walker's
// scope: only *_test.go files and // covers: lines count.
func TestMissingCoverageIgnoresNonTestFilesAndNonCoverComments(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "internal/example/a.go", "// covers: FIN-01\npackage example\n")
	writeTestFile(t, root, "internal/example/b_test.go", "package example\n\n// FIN-02 is covered elsewhere.\nfunc TestB(t *testing.T) {}\n")
	missing, err := matchscore.MissingCoverage(root, []string{"FIN-01", "FIN-02"})
	if err != nil {
		t.Fatalf("MissingCoverage: %v", err)
	}
	if !reflect.DeepEqual(missing, []string{"FIN-01", "FIN-02"}) {
		t.Fatalf("missing = %v, want [FIN-01 FIN-02]", missing)
	}
}

// TestScannerFixturesDoNotCount proves the raw-string `// covers:` fixtures in
// this file cannot satisfy the repository gate: only parsed Go comments count.
func TestScannerFixturesDoNotCount(t *testing.T) {
	root := findRepoRoot(t)
	covered, err := matchscore.Coverage(root)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	fixtureFile := filepath.ToSlash(filepath.Join("internal", "matchscore", "annotations_test.go"))
	for id, files := range covered {
		for _, file := range files {
			if file == fixtureFile {
				t.Errorf("%s is credited to %s; raw-string fixtures must not count", id, fixtureFile)
			}
		}
	}
}

// TestMissingCoverageFailsAfterRemovingRealAnnotation copies every test file to
// a temp root, then removes the real FIN-02 annotation from
// internal/money/money_test.go. The gate must report FIN-02 missing: the
// scanner's own raw-string fixtures must not mask a deleted real annotation.
func TestMissingCoverageFailsAfterRemovingRealAnnotation(t *testing.T) {
	root := findRepoRoot(t)
	tmp := t.TempDir()
	copyTestFiles(t, root, tmp)

	missing, err := matchscore.MissingCoverage(tmp, matchscore.M1CoverageSubset())
	if err != nil {
		t.Fatalf("baseline MissingCoverage: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("baseline temp copy is missing annotations: %v", missing)
	}

	moneyTest := filepath.Join(tmp, "internal", "money", "money_test.go")
	data, err := os.ReadFile(moneyTest)
	if err != nil {
		t.Fatalf("read %s: %v", moneyTest, err)
	}
	updated := strings.Replace(string(data), "FIN-02, ", "", 1)
	if updated == string(data) {
		t.Fatal("test setup: FIN-02 annotation not found in money_test.go")
	}
	if err := os.WriteFile(moneyTest, []byte(updated), 0o644); err != nil {
		t.Fatalf("write %s: %v", moneyTest, err)
	}

	missing, err = matchscore.MissingCoverage(tmp, matchscore.M1CoverageSubset())
	if err != nil {
		t.Fatalf("MissingCoverage after removal: %v", err)
	}
	if !reflect.DeepEqual(missing, []string{"FIN-02"}) {
		t.Fatalf("missing = %v, want [FIN-02]; fixtures must not mask a removed real annotation", missing)
	}
}

func copyTestFiles(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == src {
				return nil
			}
			switch entry.Name() {
			case ".git", ".team", "node_modules", "tmp", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy test files: %v", err)
	}
}

// TestM1CoverageSubset pins the documented M1-16 subset (D-8, Q-03).
func TestM1CoverageSubset(t *testing.T) {
	want := []string{
		"FIN-01", "FIN-02", "FIN-03", "FIN-04", "FIN-05", "FIN-06",
		"FIN-10", "FIN-11", "FIN-12", "FIN-13", "FIN-14",
		"FIN-50", "FIN-51", "FIN-55",
	}
	if got := matchscore.M1CoverageSubset(); !reflect.DeepEqual(got, want) {
		t.Fatalf("M1CoverageSubset = %v, want %v", got, want)
	}
	if matchscore.SubsetRationale == "" {
		t.Fatal("SubsetRationale must document why M1-16 checks a subset")
	}
}

// TestRepositoryHasM1Annotations runs the scanner over this repository. It is
// a unit test so a missing annotation fails `make test`, not only the
// integration gate.
func TestRepositoryHasM1Annotations(t *testing.T) {
	root := findRepoRoot(t)
	missing, err := matchscore.MissingCoverage(root, matchscore.M1CoverageSubset())
	if err != nil {
		t.Fatalf("MissingCoverage: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing M1 coverage annotations: %v", missing)
	}
}

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}
