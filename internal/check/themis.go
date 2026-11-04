package check

import (
	_ "embed"
	"fmt"
)

//go:embed rollout.yaml
var rollout []byte

// Themis is the checks Themis ships with, compiled against the evidence
// templates an install has.
func Themis(templates []string) (List, error) {
	defs, err := Parse(rollout)
	if err != nil {
		return nil, fmt.Errorf("parse Themis's checks: %w", err)
	}
	return Compile(defs, templates)
}
