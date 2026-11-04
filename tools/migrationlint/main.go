package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: migrationlint [dir ...]")
		fmt.Fprintln(os.Stderr, "Scans *.sql migrations for REAL, DOUBLE PRECISION, and FLOAT (FIN-01).")
	}
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"migrations"}
	}

	// Exit 1 means the lint found forbidden types; exit 2 means the scan
	// itself failed. Keeping them distinct matches go vet's convention.
	status := 0
	for _, dir := range dirs {
		findings, err := CheckDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrationlint: %v\n", err)
			status = 2
			continue
		}
		for _, finding := range findings {
			fmt.Fprintln(os.Stderr, finding.String())
		}
		if len(findings) > 0 && status == 0 {
			status = 1
		}
	}
	os.Exit(status)
}
