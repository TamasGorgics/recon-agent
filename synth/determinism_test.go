package synth_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/TamasGorgics/recon-agent/internal/clock"
	"github.com/TamasGorgics/recon-agent/synth"
)

// TestDeterministicSameSeed pins D-7: the same seed, config, and days produce
// byte-identical files across two independent generations.
func TestDeterministicSameSeed(t *testing.T) {
	opts := testOptions(42, 6)
	first := generate(t, opts)
	second := generate(t, opts)

	firstHashes := hashFiles(first.Files)
	secondHashes := hashFiles(second.Files)
	if len(firstHashes) != len(secondHashes) {
		t.Fatalf("file count differs: %d vs %d", len(firstHashes), len(secondHashes))
	}
	for path, hash := range firstHashes {
		if got, ok := secondHashes[path]; !ok {
			t.Errorf("second generation is missing %s", path)
		} else if got != hash {
			t.Errorf("%s hash = %s, want %s", path, got, hash)
		}
	}

	firstFiles := make(map[string][]byte, len(first.Files))
	for _, file := range first.Files {
		firstFiles[file.Path] = file.Data
	}
	for _, file := range second.Files {
		if !bytes.Equal(firstFiles[file.Path], file.Data) {
			t.Errorf("%s bytes differ between two generations of seed %d", file.Path, opts.Seed)
		}
	}
}

// TestGeneratedDirsAreByteIdentical covers the tester's `diff -r` check.
func TestGeneratedDirsAreByteIdentical(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	opts := testOptions(7, 4)
	if err := generate(t, opts).Write(first); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := generate(t, opts).Write(second); err != nil {
		t.Fatalf("write second: %v", err)
	}
	firstHashes := hashDir(t, first)
	secondHashes := hashDir(t, second)
	if len(firstHashes) != len(secondHashes) {
		t.Fatalf("directory file counts differ: %d vs %d", len(firstHashes), len(secondHashes))
	}
	for path, hash := range firstHashes {
		if secondHashes[path] != hash {
			t.Errorf("%s hash = %s, want %s", path, secondHashes[path], hash)
		}
	}
}

// TestInjectedClockDoesNotChangeOutput proves Generate never reads the wall
// clock: only Load may use the injected clock.
func TestInjectedClockDoesNotChangeOutput(t *testing.T) {
	opts := testOptions(11, 3)
	morning, err := synth.New(clock.NewFake(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))).Generate(opts)
	if err != nil {
		t.Fatalf("morning Generate: %v", err)
	}
	evening, err := synth.New(clock.NewFake(time.Date(2030, 12, 31, 23, 59, 59, 0, time.UTC))).Generate(opts)
	if err != nil {
		t.Fatalf("evening Generate: %v", err)
	}
	morningHashes := hashFiles(morning.Files)
	for _, file := range evening.Files {
		if morningHashes[file.Path] != hashText(string(file.Data)) {
			t.Errorf("%s changed with the injected clock", file.Path)
		}
	}
}

// TestGenerateWithoutDatabase runs generation with no database configuration,
// proving `recon synth generate` is DB-free.
func TestGenerateWithoutDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("TEST_DATABASE_URL", "")
	dataset := generate(t, testOptions(1, 1))
	if dataset.Manifest.DatasetVersion == "" {
		t.Fatal("generation produced no dataset version without a database")
	}
}

// hashDir returns the SHA-256 of every regular file under root, keyed by the
// slash-separated relative path.
func hashDir(t *testing.T, root string) map[string]string {
	t.Helper()
	hashes := make(map[string]string)
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		sum := sha256.Sum256(data)
		hashes[relative] = hex.EncodeToString(sum[:])
	}
	return hashes
}
