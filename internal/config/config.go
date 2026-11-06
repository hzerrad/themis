// Package config loads the Themis configuration file, which holds how Themis
// is installed. Profiles come from it, or from custom resources when Resources
// is set.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/invopop/jsonschema"

	"sigs.k8s.io/yaml"
)

// Provider gives the configuration in force now, which may change between
// calls. Read it once per unit of work.
type Provider interface {
	Current() Config
}

// Current makes a Config a Provider that never changes.
func (c Config) Current() Config { return c }

// Config is the root of the configuration file.
type Config struct {
	// EnrolledNamespaces are the namespaces Themis acts on; alerts about others
	// are rejected.
	EnrolledNamespaces []string `json:"enrolledNamespaces" jsonschema:"minItems=1,uniqueItems=true"`

	// Intake configures the webhook Alertmanager can send alerts to.
	Intake Intake `json:"intake"`

	// Alerting says where Themis reads alerts: Alertmanager's API.
	Alerting Alerting `json:"alerting,omitzero"`

	// Investigators configures access to the gRPC API.
	Investigators Investigators `json:"investigators"`

	// API configures access to the HTTP API and the UI.
	API API `json:"api"`

	// Executor is where approved actions run; without it Themis never writes.
	Executor *Executor `json:"executor,omitempty"`

	// Notifications announce escalations to humans.
	Notifications Notifications `json:"notifications,omitzero"`

	// Settings are those made in Themis's setup, kept in its database.
	Settings *Settings `json:"settings,omitempty"`

	// Model is what the install itself set for the investigator's model, so
	// setup leaves those alone. The chart sets it.
	Model Model `json:"model,omitzero"`

	// ExternalProposals lets callers outside Themis suggest fixes.
	ExternalProposals ExternalProposals `json:"externalProposals,omitzero"`

	// Resources reads profiles and global settings from custom resources,
	// instead of the profiles below.
	Resources *Resources `json:"resources,omitempty"`

	// Scenarios are the scenario packs Themis ships, each with its own
	// profile.
	Scenarios Scenarios `json:"scenarios,omitzero"`

	// Metrics says where Themis reads how much memory and CPU containers
	// use.
	Metrics Metrics `json:"metrics,omitzero"`

	// Profiles classify alerts into incident classes; an incident gets every
	// profile its alert matches, highest priority first. One named like a
	// scenario pack's profile replaces it.
	Profiles []Profile `json:"profiles,omitempty"`
}

// Duration is a time.Duration written like "5m".
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like 5m: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (Duration) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`, Description: "A duration such as 90s or 5m."}
}

// Load reads, parses and validates the configuration file at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator, not by a request
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates a configuration document. Unknown keys are errors.
func Parse(data []byte) (Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return c, nil
}

// WithProfiles returns c with each of defaults that none of c's profiles
// names added after c's own, so c's win: by name, and on a tie in priority,
// since the first of equal priority matches.
func (c Config) WithProfiles(defaults []Profile) Config {
	out := slices.Clone(c.Profiles)
	for _, d := range defaults {
		if !slices.ContainsFunc(c.Profiles, func(p Profile) bool { return p.Name == d.Name }) {
			out = append(out, d)
		}
	}
	c.Profiles = out
	return c
}

// IsEnrolled reports whether alerts about namespace ns may create work.
func (c Config) IsEnrolled(ns string) bool {
	return slices.Contains(c.EnrolledNamespaces, ns)
}
