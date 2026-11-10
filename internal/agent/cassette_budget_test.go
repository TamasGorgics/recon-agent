package agent_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	maxEvidenceFileBytes     = 1 << 20
	totalCassetteBudgetBytes = 10 << 20
)

var evidenceKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type cassetteRootReport struct {
	Root  string
	Files int
	Bytes int64
}

type budgetCassette struct {
	Schema         int    `json:"schema"`
	Key            string `json:"key"`
	Provider       string `json:"provider"`
	PromptVersion  string `json:"prompt_version"`
	RequestedModel string `json:"requested_model"`
	ActualModel    string `json:"actual_model"`
	Request        struct {
		Stream      *bool    `json:"stream"`
		Temperature *float64 `json:"temperature"`
	} `json:"request"`
	Redactions []string `json:"redactions"`
}

// TestCommittedEvalCassetteBudget enforces the committed cassette size and
// hygiene contract: 1 MiB per file, 10 MiB across every committed root, schema
// 1, filename == key, path model/prompt consistency, stream false,
// temperature 0, and empty redactions.
func TestCommittedEvalCassetteBudget(t *testing.T) {
	root := cassetteRepoRoot(t)
	roots := []string{
		filepath.Join(root, "eval", "cassettes"),
		filepath.Join(root, "eval", "cassette-evidence"),
	}
	baselines, err := filepath.Glob(filepath.Join(root, "eval", "baselines", "*", "cassettes"))
	if err != nil {
		t.Fatalf("glob baselines: %v", err)
	}
	roots = append(roots, baselines...)

	report, err := auditCassetteRoots(roots, maxEvidenceFileBytes, totalCassetteBudgetBytes)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range report {
		t.Logf("cassette root %s: %d file(s), %d bytes", entry.Root, entry.Files, entry.Bytes)
		total += entry.Bytes
	}
	if total > totalCassetteBudgetBytes {
		t.Fatalf("committed cassette evidence %d bytes exceeds the %d-byte budget", total, totalCassetteBudgetBytes)
	}
}

func auditCassetteRoots(roots []string, fileCap, totalCap int64) ([]cassetteRootReport, error) {
	report := make([]cassetteRootReport, 0, len(roots))
	var total int64
	for _, root := range roots {
		entry := cassetteRootReport{Root: root}
		info, err := os.Stat(root)
		if err != nil {
			if os.IsNotExist(err) {
				report = append(report, entry)
				continue
			}
			return nil, fmt.Errorf("stat cassette root %s: %w", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("cassette root %s is not a directory", root)
		}
		err = filepath.WalkDir(root, func(path string, dirent fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if dirent.IsDir() {
				return nil
			}
			fileInfo, err := dirent.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			entry.Files++
			entry.Bytes += fileInfo.Size()
			total += fileInfo.Size()
			if fileInfo.Size() > fileCap {
				return fmt.Errorf("cassette %s is %d bytes, per-file cap is %d", path, fileInfo.Size(), fileCap)
			}
			if !strings.HasSuffix(dirent.Name(), ".json") {
				return nil
			}
			return validateEvidenceCassette(root, path)
		})
		if err != nil {
			return nil, fmt.Errorf("root %s: %w", root, err)
		}
		report = append(report, entry)
	}
	if total > totalCap {
		var breakdown strings.Builder
		for _, entry := range report {
			fmt.Fprintf(&breakdown, "\n  %s: %d file(s), %d bytes", entry.Root, entry.Files, entry.Bytes)
		}
		return nil, fmt.Errorf("committed cassette evidence is %d bytes, over the %d-byte budget; per-root breakdown:%s",
			total, totalCap, breakdown.String())
	}
	return report, nil
}

func validateEvidenceCassette(root, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read cassette %s: %w", path, err)
	}
	if match := forbiddenCassettePattern.FindString(string(data)); match != "" {
		return fmt.Errorf("cassette %s contains forbidden material %q", path, match)
	}
	var doc budgetCassette
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode cassette %s: %w", path, err)
	}
	if doc.Schema != 1 {
		return fmt.Errorf("cassette %s schema = %d, want 1", path, doc.Schema)
	}
	if !evidenceKeyPattern.MatchString(doc.Key) {
		return fmt.Errorf("cassette %s key = %q, want 64 lowercase hex", path, doc.Key)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("relative path for %s: %w", path, err)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 {
		return fmt.Errorf("cassette %s path = %q, want <prefix>/<provider>/<model>/<prompt>/<key>.json", path, rel)
	}
	provider := parts[len(parts)-4]
	model := parts[len(parts)-3]
	prompt := parts[len(parts)-2]
	filename := parts[len(parts)-1]
	if provider != doc.Provider {
		return fmt.Errorf("cassette %s path provider %q != document provider %q", path, provider, doc.Provider)
	}
	if model != doc.RequestedModel {
		return fmt.Errorf("cassette %s path model %q != requested_model %q", path, model, doc.RequestedModel)
	}
	if prompt != doc.PromptVersion {
		return fmt.Errorf("cassette %s path prompt %q != prompt_version %q", path, prompt, doc.PromptVersion)
	}
	if filename != doc.Key+".json" {
		return fmt.Errorf("cassette %s filename %q does not match key %q", path, filename, doc.Key)
	}
	if doc.ActualModel == "" {
		return fmt.Errorf("cassette %s actual_model is empty", path)
	}
	if doc.Request.Stream == nil || *doc.Request.Stream {
		return fmt.Errorf("cassette %s request.stream = %v, want false", path, doc.Request.Stream)
	}
	if doc.Request.Temperature == nil || *doc.Request.Temperature != 0 {
		return fmt.Errorf("cassette %s request.temperature = %v, want 0", path, doc.Request.Temperature)
	}
	if len(doc.Redactions) != 0 {
		return fmt.Errorf("cassette %s redactions = %v, want empty for committed synthetic evidence", path, doc.Redactions)
	}
	return nil
}

func writeEvidenceCassette(t *testing.T, root, provider, model, prompt, key string, mutate func(map[string]any)) string {
	t.Helper()
	stream := false
	temperature := 0.0 //floatlint:allow provider temperature, not money
	doc := map[string]any{
		"schema":          1,
		"key":             key,
		"provider":        provider,
		"prompt_version":  prompt,
		"requested_model": model,
		"actual_model":    model,
		"request": map[string]any{
			"stream":      stream,
			"temperature": temperature,
		},
		"redactions": []string{},
	}
	if mutate != nil {
		mutate(doc)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal cassette: %v", err)
	}
	dir := filepath.Join(root, provider, model, prompt)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir cassette dir: %v", err)
	}
	path := filepath.Join(dir, key+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write cassette: %v", err)
	}
	return path
}

func evidenceKey(label string) string {
	sum := sha256.Sum256([]byte(label))
	return fmt.Sprintf("%x", sum)
}

func TestCassetteBudgetFailsWithPerRootBreakdown(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeEvidenceCassette(t, rootA, "opencode", "glm-5.3-flash", "v1", evidenceKey("a"), nil)
	writeEvidenceCassette(t, rootB, "opencode", "deepseek-v4.1-flash", "v2", evidenceKey("b"), nil)

	report, err := auditCassetteRoots([]string{rootA, rootB}, maxEvidenceFileBytes, 10<<20)
	if err != nil {
		t.Fatalf("audit within budget: %v", err)
	}
	var total int64
	for _, entry := range report {
		total += entry.Bytes
	}
	if total <= 0 {
		t.Fatal("audit reported no bytes")
	}
	_, err = auditCassetteRoots([]string{rootA, rootB}, maxEvidenceFileBytes, total-1)
	if err == nil {
		t.Fatal("audit accepted a total over budget")
	}
	for _, want := range []string{"over the", "per-root breakdown", rootA, rootB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("budget error is missing %q: %v", want, err)
		}
	}

	if _, err := auditCassetteRoots([]string{rootA}, 32, 10<<20); err == nil ||
		!strings.Contains(err.Error(), "per-file cap") {
		t.Fatalf("per-file cap error = %v", err)
	}
}

func TestCassetteBudgetIgnoresMissingRoots(t *testing.T) {
	report, err := auditCassetteRoots([]string{
		filepath.Join(t.TempDir(), "missing"),
	}, maxEvidenceFileBytes, totalCassetteBudgetBytes)
	if err != nil {
		t.Fatalf("audit missing root: %v", err)
	}
	if len(report) != 1 || report[0].Files != 0 {
		t.Fatalf("report = %+v, want one empty entry", report)
	}
}

func TestCassetteEvidenceHygiene(t *testing.T) {
	tests := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"wrong schema": {
			mutate: func(doc map[string]any) { doc["schema"] = 2 },
			want:   "schema",
		},
		"bad key": {
			mutate: func(doc map[string]any) { doc["key"] = "not-a-key" },
			want:   "key",
		},
		"path model mismatch": {
			mutate: func(doc map[string]any) { doc["requested_model"] = "other-model" },
			want:   "path model",
		},
		"path prompt mismatch": {
			mutate: func(doc map[string]any) { doc["prompt_version"] = "v9" },
			want:   "path prompt",
		},
		"stream true": {
			mutate: func(doc map[string]any) {
				doc["request"].(map[string]any)["stream"] = true
			},
			want: "stream",
		},
		"temperature nonzero": {
			mutate: func(doc map[string]any) {
				doc["request"].(map[string]any)["temperature"] = 0.7 //floatlint:allow provider temperature, not money
			},
			want: "temperature",
		},
		"redactions present": {
			mutate: func(doc map[string]any) { doc["redactions"] = []string{"api_key"} },
			want:   "redactions",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			key := evidenceKey(name)
			writeEvidenceCassette(t, root, "opencode", "glm-5.3-flash", "v1", key, test.mutate)
			_, err := auditCassetteRoots([]string{root}, maxEvidenceFileBytes, totalCassetteBudgetBytes)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("audit = %v, want error containing %q", err, test.want)
			}
		})
	}
}
