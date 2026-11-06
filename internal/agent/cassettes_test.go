package agent_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const maxCommittedCassetteBytes = 1 << 20

// wantM2CassetteFiles is the recorded M2 count: the three scenarios produced
// 11 model interactions. Re-record only through the documented live test and
// update this constant in the same change.
const wantM2CassetteFiles = 11

var forbiddenCassettePattern = regexp.MustCompile(`(?i)oc_sk_|Bearer\s|ses_|Authorization|api_key`)

type committedCassette struct {
	Schema        int    `json:"schema"`
	Key           string `json:"key"`
	Provider      string `json:"provider"`
	PromptVersion string `json:"prompt_version"`
	ActualModel   string `json:"actual_model"`
	Request       struct {
		Stream      *bool    `json:"stream"`
		Temperature *float64 `json:"temperature"`
	} `json:"request"`
	Redactions []string `json:"redactions"`
}

// TestCommittedCassettes is the key-free guard on the recorded M2 fixtures: no
// secret material, no session ids, no redaction classes, request temperature 0,
// stream false, and the documented provider/model/prompt path.
func TestCommittedCassettes(t *testing.T) {
	dir := filepath.Join(cassetteRepoRoot(t), "eval", "cassettes")
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(files) != wantM2CassetteFiles {
		t.Fatalf("committed cassettes = %d under %s, want %d (M2 has one file per model interaction; missing scenarios mean a re-record is needed)",
			len(files), dir, wantM2CassetteFiles)
	}

	for _, file := range files {
		file := file
		t.Run(strings.TrimPrefix(file, dir+string(filepath.Separator)), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read cassette: %v", err)
			}
			if len(data) > maxCommittedCassetteBytes {
				t.Fatalf("cassette is %d bytes, cap is %d", len(data), maxCommittedCassetteBytes)
			}
			if match := forbiddenCassettePattern.FindString(string(data)); match != "" {
				t.Fatalf("cassette contains forbidden material %q", match)
			}
			var doc committedCassette
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("decode cassette: %v", err)
			}
			if doc.Schema != 1 {
				t.Fatalf("schema = %d, want 1", doc.Schema)
			}
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(doc.Key) {
				t.Fatalf("key = %q, want 64 lowercase hex", doc.Key)
			}
			if doc.Provider != "opencode" {
				t.Fatalf("provider = %q, want opencode", doc.Provider)
			}
			if doc.PromptVersion != "v1" {
				t.Fatalf("prompt version = %q, want v1", doc.PromptVersion)
			}
			if doc.ActualModel == "" {
				t.Fatal("actual_model is empty")
			}
			if doc.Request.Stream == nil || *doc.Request.Stream {
				t.Fatalf("request.stream = %v, want false", doc.Request.Stream)
			}
			if doc.Request.Temperature == nil || *doc.Request.Temperature != 0 {
				t.Fatalf("request.temperature = %v, want 0", doc.Request.Temperature)
			}
			if len(doc.Redactions) != 0 {
				t.Fatalf("redactions = %v, want empty for synthetic fixture data", doc.Redactions)
			}
			rel, err := filepath.Rel(dir, file)
			if err != nil {
				t.Fatalf("relative cassette path: %v", err)
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) != 4 || parts[0] != "opencode" || parts[2] != "v1" {
				t.Fatalf("cassette path = %q, want <provider>/<model>/v1/<hash>.json", rel)
			}
			if strings.TrimSuffix(parts[3], ".json") != doc.Key {
				t.Fatalf("file name %q does not match key %q", parts[3], doc.Key)
			}
		})
	}
}

func cassetteRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
