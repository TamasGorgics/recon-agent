package main

import (
	"strings"
	"testing"
)

func TestRunRecordUsageErrors(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	cases := map[string][]string{
		"record no key":          {"--llm", "record", "--max-cost-usd", "0.25"},
		"record negative cap":    {"--llm", "record", "--max-cost-usd", "-0.01"},
		"update with live":       {"--llm", "live", "--max-cost-usd", "1", "--update-cassettes"},
		"update with cassette":   {"--llm", "cassette", "--update-cassettes"},
		"cassette-dir with live": {"--llm", "live", "--max-cost-usd", "1", "--cassette-dir", "x"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, errOut := runCLI(t, append([]string{"run"}, args...)...)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, errOut)
			}
		})
	}

	// The cap guard is reached only with a key present; it must still exit 2
	// and must never echo the key value.
	const secret = "oc_sk_cli_record_secret"
	t.Setenv("OPENCODE_API_KEY", secret)
	code, _, errOut := runCLI(t, "run", "--llm", "record", "--max-cost-usd", "0")
	if code != 2 {
		t.Fatalf("record zero cap code = %d, want 2", code)
	}
	if strings.Contains(errOut, secret) {
		t.Fatal("record validation error leaked OPENCODE_API_KEY")
	}
}

func TestHelpMentionsRecordMode(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"run", "-h"}} {
		code, stdout, stderr := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("%v code = %d", args, code)
		}
		text := stdout + stderr
		for _, phrase := range []string{"record", "--cassette-dir", "--update-cassettes"} {
			if !strings.Contains(text, phrase) {
				t.Errorf("%v help is missing %q", args, phrase)
			}
		}
	}
}
