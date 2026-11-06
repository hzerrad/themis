// Command gen-alert-rules writes the alerting rules Themis ships into the Helm
// chart, which installs them as a PrometheusRule.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hzerrad/themis/internal/incident/intake/rules"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-alert-rules <output.yaml>")
		os.Exit(2)
	}
	out := os.Args[1]
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // a directory in the repository
		fmt.Fprintln(os.Stderr, "gen-alert-rules:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, rules.File, 0o644); err != nil { //nolint:gosec // a file in the repository
		fmt.Fprintln(os.Stderr, "gen-alert-rules:", err)
		os.Exit(1)
	}
}
