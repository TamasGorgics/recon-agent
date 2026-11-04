package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDemoSeedHelp pins M1-16's --reset documentation: asking for help must
// print a real usage block (not "flag: help requested"), document the
// destructive dev-only reset, and exit successfully.
func TestDemoSeedHelp(t *testing.T) {
	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"seed", "--help"}, &out)
	if err != nil {
		t.Fatalf("demo seed --help: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo seed --help exit code = %d, want 0", code)
	}

	help := out.String()
	for _, want := range []string{
		"usage: recon demo seed",
		"--reset",
		"drop and recreate the public schema",
		"dev only",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not contain %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", help)
	}
}

// TestDemoSeedHelpDoesNotRequireConfig pins that help is handled before config
// loading: it must work with an unset DATABASE_URL and in production.
func TestDemoSeedHelpDoesNotRequireConfig(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RECON_ENV", "production")

	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"seed", "-h"}, &out)
	if err != nil {
		t.Fatalf("demo seed -h: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo seed -h exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage: recon demo seed") {
		t.Fatalf("help output missing usage block:\n%s", out.String())
	}
}

// TestHelpListsCommands pins the top-level command list, including the M1
// demo, synth and tieout commands.
func TestHelpListsCommands(t *testing.T) {
	var out bytes.Buffer
	err := runTo(context.Background(), []string{"help"}, &out)
	if err != nil {
		t.Fatalf("recon help: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("recon help exit code = %d, want 0", code)
	}

	help := out.String()
	for _, want := range []string{"demo seed", "migrate up", "synth generate", "synth load", "testdb clean", "tieout"} {
		if !strings.Contains(help, want) {
			t.Errorf("recon help does not list %q:\n%s", want, help)
		}
	}
}
