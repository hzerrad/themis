// Package scenarios holds the scenario packs Themis ships, one folder each:
// docs/scenarios.md says how to write one. Only what Themis reads at start-up
// is built into the binary.
package scenarios

import "embed"

// FS holds each pack's scenario.yaml and checks.yaml, under the pack's name.
//
//go:embed */scenario.yaml */checks.yaml
var FS embed.FS
