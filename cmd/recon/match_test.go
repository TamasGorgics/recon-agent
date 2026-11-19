package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMatchHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runMatchTo(context.Background(), []string{"unmatch", "--help"}, &out); err != nil {
		t.Fatalf("match unmatch --help: %v", err)
	}
	help := out.String()
	for _, want := range []string{
		"usage: recon match unmatch",
		"--id <match-uuid>",
		"--reason <text>",
		"--actor human:<id>",
		"--json",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not contain %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", help)
	}

	out.Reset()
	if err := runMatchTo(context.Background(), []string{"help"}, &out); err != nil {
		t.Fatalf("match help: %v", err)
	}
	if !strings.Contains(out.String(), "Commands:") {
		t.Errorf("match help missing command list:\n%s", out.String())
	}
}

func TestMatchUnmatchFlagValidationIsUsageExitTwo(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_APP_URL", "")

	validID := "44444444-4444-4444-8444-444444444444"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown flag", args: []string{"unmatch", "--nope"},
			want: "flag provided but not defined"},
		{name: "missing id",
			args: []string{"unmatch", "--reason", "operator requested", "--actor", "human:alice"},
			want: "--id, --reason and --actor are required"},
		{name: "bad id",
			args: []string{"unmatch", "--id", "not-a-uuid", "--reason", "operator requested",
				"--actor", "human:alice"},
			want: "not a canonical UUID"},
		{name: "missing reason",
			args: []string{"unmatch", "--id", validID, "--actor", "human:alice"},
			want: "--id, --reason and --actor are required"},
		{name: "blank reason",
			args: []string{"unmatch", "--id", validID, "--reason", "   ",
				"--actor", "human:alice"},
			want: "unmatch reason is required"},
		{name: "overlong reason",
			args: []string{"unmatch", "--id", validID, "--reason", strings.Repeat("a", 501),
				"--actor", "human:alice"},
			want: "exceeds 500 bytes"},
		{name: "missing actor",
			args: []string{"unmatch", "--id", validID, "--reason", "operator requested"},
			want: "--id, --reason and --actor are required"},
		{name: "actor without prefix",
			args: []string{"unmatch", "--id", validID, "--reason", "operator requested",
				"--actor", "alice"},
			want: "has no type prefix"},
		{name: "system actor",
			args: []string{"unmatch", "--id", validID, "--reason", "operator requested",
				"--actor", "system:reconciler"},
			want: "requires a human actor"},
		{name: "agent actor",
			args: []string{"unmatch", "--id", validID, "--reason", "operator requested",
				"--actor", "agent:model-v1"},
			want: "requires a human actor"},
		{name: "unexpected argument",
			args: []string{"unmatch", "--id", validID, "--reason", "operator requested",
				"--actor", "human:alice", "extra"},
			want: "unexpected argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runMatchTo(context.Background(), tc.args, &out)
			if err == nil {
				t.Fatal("runMatchTo() = nil, want an error")
			}
			if code := exitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (%v)", code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestMatchUnmatchRequiresAppURL(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_APP_URL", "")

	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", "44444444-4444-4444-8444-444444444444",
		"--reason", "operator requested",
		"--actor", "human:alice",
	}, &out)
	if err == nil {
		t.Fatal("runMatchTo() = nil, want a missing app_url error")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "database.app_url") ||
		!strings.Contains(err.Error(), "DATABASE_APP_URL") {
		t.Fatalf("error = %q, want it to name database.app_url/DATABASE_APP_URL", err)
	}
}

// TestMatchUnmatchAllowsAppURLOnly pins the admin-host contract: the command
// must not require the migration database.url. A malformed app DSN is enough
// to prove config loading passed the AppURL into the role-pool opener.
func TestMatchUnmatchAllowsAppURLOnly(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_APP_URL", "not-a-dsn")

	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{
		"unmatch",
		"--id", "44444444-4444-4444-8444-444444444444",
		"--reason", "operator requested",
		"--actor", "human:alice",
	}, &out)
	if err == nil {
		t.Fatal("runMatchTo() = nil, want a bad-DSN error")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 (%v)", code, err)
	}
	if strings.Contains(err.Error(), "database.url") {
		t.Fatalf("AppURL-only config was rejected for database.url: %v", err)
	}
	if !strings.Contains(err.Error(), "application role") {
		t.Fatalf("error = %q, want the application-role open to be reached", err)
	}
}

func TestMatchUnknownSubcommandIsUsageExitTwo(t *testing.T) {
	var out bytes.Buffer
	err := runMatchTo(context.Background(), []string{"reverse"}, &out)
	if err == nil {
		t.Fatal("runMatchTo() = nil, want an unknown command error")
	}
	if code := exitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(err.Error(), "unknown match command") {
		t.Fatalf("error = %q, want unknown match command", err)
	}
}

// TestReverseCommandIsNotRegistered pins that only the refusal message may
// mention the future recon reverse command; M4-10 does not add it.
func TestReverseCommandIsNotRegistered(t *testing.T) {
	var out bytes.Buffer
	err := runTo(context.Background(), []string{"reverse"}, &out)
	if err == nil {
		t.Fatal("runTo(reverse) = nil, want an unknown command error")
	}
	if code := exitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), `unknown command "reverse"`) {
		t.Fatalf("error = %q, want an unknown command", err)
	}
}
