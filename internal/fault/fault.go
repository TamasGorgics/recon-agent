//go:build faultinject

package fault

import (
	"os"
	"strings"
)

// exitCode is the process status a matching "exit" failpoint uses. Tests
// assert this exact value.
const exitCode = 86

// Hit parses RECON_FAULT as "<point>:<action>". A point equal to name with
// action "exit" terminates the process with status 86; action "panic" panics
// with the point name. An empty, malformed, or non-matching value is a no-op.
func Hit(name string) {
	point, action, ok := strings.Cut(os.Getenv("RECON_FAULT"), ":")
	if !ok || point != name {
		return
	}
	switch action {
	case "exit":
		os.Exit(exitCode)
	case "panic":
		panic(name)
	}
}
