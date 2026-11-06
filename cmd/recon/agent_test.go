package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestAgentRunHelp pins that help is handled before config or flag errors.
func TestAgentRunHelp(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")

	var out bytes.Buffer
	err := runAgentTo(context.Background(), []string{"run", "--help"}, &out)
	if err != nil {
		t.Fatalf("agent run --help: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{
		"usage: recon agent run",
		"--case",
		"--llm scripted|opencode",
		"replays committed cassettes",
		"OPENCODE_API_KEY",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output does not contain %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", out.String())
	}
}

// TestAgentRunFlagValidation pins that bad flags and prompts fail before any
// configuration, database, or model work.
func TestAgentRunFlagValidation(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("OPENCODE_API_KEY", "")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing case",
			args: []string{"run", "--llm", "scripted"},
			want: "--case <uuid>",
		},
		{
			name: "invalid case",
			args: []string{"run", "--case", "not-a-uuid", "--llm", "scripted"},
			want: "--case must be a UUID",
		},
		{
			name: "unknown llm",
			args: []string{"run", "--case", "10000000-0000-4000-8000-000000000001", "--llm", "nope"},
			want: "scripted or opencode",
		},
		{
			name: "unknown prompt",
			args: []string{"run", "--case", "10000000-0000-4000-8000-000000000001", "--llm", "scripted", "--prompt", "v99"},
			want: "unknown version",
		},
		{
			name: "live without key",
			args: []string{"run", "--case", "10000000-0000-4000-8000-000000000001", "--llm", "opencode"},
			want: "OPENCODE_API_KEY",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runAgentTo(context.Background(), tc.args, &out)
			if err == nil {
				t.Fatalf("runAgentTo() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "oc_sk_") {
				t.Fatalf("error leaks an API key: %v", err)
			}
		})
	}
}
