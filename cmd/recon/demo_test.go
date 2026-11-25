package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDemoQualityHelp pins the M5-05 surface: help describes the synthetic
// quality layer, the no-network/no-LLM boundary, the fail-closed reset, and
// the M1 + ops + quality layering before any config or database access.
func TestDemoQualityHelp(t *testing.T) {
	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"quality", "--help"}, &out)
	if err != nil {
		t.Fatalf("demo quality --help: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo quality --help exit code = %d, want 0", code)
	}

	help := out.String()
	for _, want := range []string{
		"usage: recon demo quality",
		"--reset",
		"guarded dev-only schema reset",
		"untagged data",
		"all-or-nothing",
		"quality layer",
		"SYNTHETIC DEMO DATA — NOT PRODUCTION EVIDENCE",
		"no network",
		"no LLM",
		"existing eval or demo-quality rows",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not contain %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", help)
	}
}

// TestDemoQualityHelpDoesNotRequireConfig pins that quality help is handled
// before config loading and before the production reset guard.
func TestDemoQualityHelpDoesNotRequireConfig(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RECON_ENV", "production")

	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"quality", "-h"}, &out)
	if err != nil {
		t.Fatalf("demo quality -h: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo quality -h exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage: recon demo quality") {
		t.Fatalf("help output missing usage block:\n%s", out.String())
	}
}

// TestDemoUnknownCommandMentionsQuality pins the usage hint after the new
// subcommand was added.
func TestDemoUnknownCommandMentionsQuality(t *testing.T) {
	err := runDemoTo(context.Background(), []string{"nope"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Fatalf("unknown demo command error = %v, want a quality usage hint", err)
	}
}

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
		"guarded dev-only schema reset",
		"untagged data",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not contain %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", help)
	}
}

// TestDemoOpsHelp pins the M5-04 surface: help describes the synthetic ops
// layer and the dev-only reset before any config or database access.
func TestDemoOpsHelp(t *testing.T) {
	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"ops", "--help"}, &out)
	if err != nil {
		t.Fatalf("demo ops --help: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo ops --help exit code = %d, want 0", code)
	}

	help := out.String()
	for _, want := range []string{
		"usage: recon demo ops",
		"--reset",
		"guarded dev-only schema reset",
		"untagged data",
		"synthetic",
		"no network",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help output does not contain %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "flag: help requested") {
		t.Errorf("help output leaks the flag package error:\n%s", help)
	}
}

// TestDemoOpsHelpDoesNotRequireConfig pins that demo ops help is handled
// before config loading and before the production reset guard.
func TestDemoOpsHelpDoesNotRequireConfig(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("RECON_ENV", "production")

	var out bytes.Buffer
	err := runDemoTo(context.Background(), []string{"ops", "-h"}, &out)
	if err != nil {
		t.Fatalf("demo ops -h: %v", err)
	}
	if code := exitCode(err); code != 0 {
		t.Fatalf("demo ops -h exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "usage: recon demo ops") {
		t.Fatalf("help output missing usage block:\n%s", out.String())
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
	for _, want := range []string{"demo seed", "demo ops", "demo quality", "migrate up", "synth generate", "synth load", "testdb clean", "tieout"} {
		if !strings.Contains(help, want) {
			t.Errorf("recon help does not list %q:\n%s", want, help)
		}
	}
}
