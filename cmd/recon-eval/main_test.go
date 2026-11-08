package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMainHelpAndUnknown(t *testing.T) {
	code, out, errOut := runCLI(t, "help")
	if code != 0 || !strings.Contains(out, "recon-eval run") {
		t.Fatalf("help = code %d out %q err %q", code, out, errOut)
	}
	code, _, errOut = runCLI(t, "bogus")
	if code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown = code %d err %q", code, errOut)
	}
}

func TestRunHelp(t *testing.T) {
	code, _, errOut := runCLI(t, "run", "-h")
	if code != 0 {
		t.Fatalf("run -h code = %d", code)
	}
	if !strings.Contains(errOut, "usage: recon-eval run") {
		t.Fatalf("run -h output = %q", errOut)
	}
}

// TestRunUsageErrors proves every flag/usage error exits 2 before database or
// network work.
func TestRunUsageErrors(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	cases := map[string][]string{
		"unknown suite":      {"--suite", "matcher"},
		"empty suite":        {"--suite", ""},
		"unknown split":      {"--split", "prod"},
		"empty split":        {"--split", ""},
		"repeat zero":        {"--repeat", "0"},
		"repeat negative":    {"--repeat", "-1"},
		"concurrency zero":   {"--concurrency", "0"},
		"concurrency high":   {"--concurrency", "33"},
		"llm scripted":       {"--llm", "scripted"},
		"cost negative":      {"--max-cost-usd", "-0.01"},
		"cost not decimal":   {"--max-cost-usd", "one"},
		"seed not integer":   {"--seed", "x"},
		"seed negative":      {"--seed", "-3"},
		"resume malformed":   {"--resume", "not-a-uuid"},
		"resume with repeat": {"--resume", "11111111-1111-4111-8111-111111111111", "--repeat", "2"},
		"live no key":        {"--llm", "live", "--max-cost-usd", "1"},
		"live zero cap":      {"--llm", "live", "--max-cost-usd", "0"},
		"unknown model":      {"--model", "no-such-model"},
		"unknown prompt":     {"--prompt", "v99"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(t, append([]string{"run"}, args...)...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, errOut)
			}
		})
	}
}
