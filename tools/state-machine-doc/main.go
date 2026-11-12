// Command state-machine-doc renders the proposal transition table to Markdown.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TamasGorgics/recon-agent/internal/approval/state"
)

func main() {
	out := flag.String("out", "", "output markdown file (required)")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "state-machine-doc: -out is required")
		os.Exit(2)
	}
	if err := write(*out); err != nil {
		fmt.Fprintf(os.Stderr, "state-machine-doc: %v\n", err)
		os.Exit(1)
	}
}

func write(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	if err := os.WriteFile(path, state.RenderMarkdown(), 0o644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
