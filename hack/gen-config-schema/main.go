// Command gen-config-schema writes the JSON Schema for the Themis
// configuration file, generated from the Go types in internal/config.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"

	"github.com/hzerrad/themis/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-config-schema <output.json>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "gen-config-schema:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	r := &jsonschema.Reflector{
		// Every field is required unless its JSON tag says omitempty.
		RequiredFromJSONSchemaTags: false,
		DoNotReference:             true,
	}
	if err := r.AddGoComments("github.com/hzerrad/themis", "./internal/config"); err != nil {
		return fmt.Errorf("read doc comments: %w", err)
	}
	s := r.Reflect(&config.Config{})

	// The supported kinds live in Go; copy them in rather than repeating them
	// in a struct tag.
	kinds := s.Properties.Value("profiles").Items.Properties.Value("match").Properties.Value("targetKinds").Items
	for _, k := range config.SupportedTargetKinds {
		kinds.Enum = append(kinds.Enum, k)
	}
	s.ID = "https://github.com/hzerrad/themis/deploy/config/themis.schema.json"
	s.Title = "Themis configuration"

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(data, '\n'), 0o644) //nolint:gosec // a public schema committed to the repo
}
