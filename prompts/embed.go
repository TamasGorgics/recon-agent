// Package prompts loads versioned, embedded system prompts for the agent loop.
//
// A released version is immutable. Add a version by copying the newest
// directory, never by editing one that has ever recorded a cassette or run.
package prompts

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Version pattern accepted by Load. Versions are directories under prompts/.
var versionPattern = regexp.MustCompile(`^v[0-9]+$`)

// ErrInvalidVersion means the version name is not a vN directory name.
var ErrInvalidVersion = errors.New("prompts: invalid version")

// ErrUnknownVersion means no embedded directory matches the requested version.
var ErrUnknownVersion = errors.New("prompts: unknown version")

//go:embed v*/*.md
var embedded embed.FS

// Prompt is one composed system prompt and its parts. Composed is exactly
// System + "\n\n" + Rules with trailing whitespace trimmed from each part.
type Prompt struct {
	Version  string
	System   string
	Rules    string
	Composed string
}

// Load returns the prompt for version. It validates the version name before
// reading the embedded files and returns a typed error for invalid or unknown
// versions.
func Load(version string) (Prompt, error) {
	version = strings.TrimSpace(version)
	if !versionPattern.MatchString(version) {
		return Prompt{}, fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	systemPath := path.Join(version, "system.md")
	rulesPath := path.Join(version, "rules.md")
	system, err := fs.ReadFile(embedded, systemPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Prompt{}, fmt.Errorf("%w: %q", ErrUnknownVersion, version)
		}
		return Prompt{}, fmt.Errorf("prompts: read %s: %w", systemPath, err)
	}
	rules, err := fs.ReadFile(embedded, rulesPath)
	if err != nil {
		return Prompt{}, fmt.Errorf("prompts: read %s: %w", rulesPath, err)
	}
	systemText := strings.TrimRight(string(system), " \t\r\n")
	rulesText := strings.TrimRight(string(rules), " \t\r\n")
	if systemText == "" || rulesText == "" {
		return Prompt{}, fmt.Errorf("prompts: %s has an empty prompt part", version)
	}
	return Prompt{
		Version:  version,
		System:   systemText,
		Rules:    rulesText,
		Composed: systemText + "\n\n" + rulesText,
	}, nil
}

// Versions returns the embedded versions sorted ascending.
func Versions() ([]string, error) {
	entries, err := fs.ReadDir(embedded, ".")
	if err != nil {
		return nil, fmt.Errorf("prompts: list versions: %w", err)
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && versionPattern.MatchString(entry.Name()) {
			versions = append(versions, entry.Name())
		}
	}
	return versions, nil
}
