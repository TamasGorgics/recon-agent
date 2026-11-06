package prompts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadV1(t *testing.T) {
	prompt, err := Load("v1")
	if err != nil {
		t.Fatalf("Load(v1): %v", err)
	}
	if prompt.Version != "v1" {
		t.Fatalf("Version = %q, want v1", prompt.Version)
	}
	if prompt.Composed != prompt.System+"\n\n"+prompt.Rules {
		t.Fatal("Composed is not exactly system + two newlines + rules")
	}
	if strings.TrimRight(prompt.Composed, " \t\r\n") != prompt.Composed {
		t.Fatal("Composed has trailing whitespace")
	}
}

func TestLoadRejectsVersions(t *testing.T) {
	for _, version := range []string{"", "1", "v", "v1/", "../v1", "v1.md", "release"} {
		t.Run(version, func(t *testing.T) {
			_, err := Load(version)
			if !errors.Is(err, ErrInvalidVersion) && !errors.Is(err, ErrUnknownVersion) {
				t.Fatalf("Load(%q) error = %v, want a typed version error", version, err)
			}
		})
	}
	if _, err := Load("v99"); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("Load(v99) error = %v, want ErrUnknownVersion", err)
	}
}

func TestV1RequiredRules(t *testing.T) {
	prompt, err := Load("v1")
	if err != nil {
		t.Fatalf("Load(v1): %v", err)
	}
	required := []string{
		"UNTRUSTED",
		"not instructions",
		"Do not guess",
		"evidence",
		"one proposal",
		"minor units",
		"risk tier",
		"Never post to the ledger",
		"allowlist",
	}
	for _, phrase := range required {
		if !strings.Contains(prompt.Composed, phrase) {
			t.Errorf("v1 composed prompt is missing %q", phrase)
		}
	}
}

func TestV1ContainsNoWallClockText(t *testing.T) {
	prompt, err := Load("v1")
	if err != nil {
		t.Fatalf("Load(v1): %v", err)
	}
	for _, text := range []string{
		time.Now().UTC().Format("2006-01-02"),
		time.Now().Format("2006-01-02"),
	} {
		if strings.Contains(prompt.Composed, text) {
			t.Fatalf("composed prompt contains wall-clock text %q", text)
		}
	}
}

func TestREADMEHasVersionProcedure(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(data)
	for _, phrase := range []string{"never edit a released version", "Re-record every cassette", "untrusted-data"} {
		if !strings.Contains(readme, phrase) {
			t.Errorf("README is missing %q", phrase)
		}
	}
}

func TestVersions(t *testing.T) {
	versions, err := Versions()
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) == 0 || versions[0] != "v1" {
		t.Fatalf("Versions = %v, want v1 first", versions)
	}
}
