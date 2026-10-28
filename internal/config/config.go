// Package config loads the Themis configuration file. It is shaped like the
// custom resources that will replace it.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/invopop/jsonschema"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/incident"
)

// TargetKindPattern matches Kind.group, with no group for the core group:
// Deployment.apps, Rollout.argoproj.io, PersistentVolumeClaim.
const TargetKindPattern = `^[A-Z][A-Za-z0-9]*(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)?$`

var targetKindRE = regexp.MustCompile(TargetKindPattern)

// IsTargetKind reports whether s is a well-formed Kind.group.
func IsTargetKind(s string) bool {
	return targetKindRE.MatchString(s)
}

// Config is the root of the configuration file.
type Config struct {
	// EnrolledNamespaces are the namespaces Themis acts on; alerts about others
	// are rejected.
	EnrolledNamespaces []string `json:"enrolledNamespaces" jsonschema:"minItems=1,uniqueItems=true"`

	// Intake configures how alerts reach Themis.
	Intake Intake `json:"intake"`

	// Profiles classify alerts into incident classes; the highest priority match
	// wins.
	Profiles []Profile `json:"profiles" jsonschema:"minItems=1"`
}

// Intake configures the Alertmanager webhook.
type Intake struct {
	// BearerTokenFile holds the token Alertmanager must send as a bearer token.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`
}

// Profile is one incident class and the alerts that belong to it.
type Profile struct {
	// Name is a DNS label, for example rollout-regression.
	Name string `json:"name" jsonschema:"minLength=1,maxLength=63"`

	// Priority orders profiles matching the same alert; higher wins. Unique,
	// from 1.
	Priority int `json:"priority" jsonschema:"minimum=1"`

	// Match decides which alerts belong to this profile.
	Match Match `json:"match"`

	// Evidence lists the templates investigations of this profile may use.
	Evidence Evidence `json:"evidence"`

	// Capabilities are the remediations proposals may name; none means
	// diagnosis only.
	Capabilities []string `json:"capabilities,omitempty" jsonschema:"uniqueItems=true"`

	// Investigation bounds each investigation; zero fields take defaults.
	Investigation Investigation `json:"investigation,omitzero"`
}

// Evidence names evidence templates.
type Evidence struct {
	// Initial templates are collected before the investigation starts.
	Initial []string `json:"initial,omitempty" jsonschema:"uniqueItems=true"`

	// Allowed templates may be requested during the investigation.
	Allowed []string `json:"allowed" jsonschema:"minItems=1,uniqueItems=true"`
}

// Investigation bounds an investigation.
type Investigation struct {
	MaxSteps            int      `json:"maxSteps,omitempty" jsonschema:"minimum=1"`
	MaxEvidenceRequests int      `json:"maxEvidenceRequests,omitempty" jsonschema:"minimum=1"`
	Timeout             Duration `json:"timeout,omitzero"`
}

// Investigation defaults.
const (
	DefaultMaxSteps             = 8
	DefaultMaxEvidenceRequests  = 6
	DefaultInvestigationTimeout = 5 * time.Minute
)

// KnownCapabilities are the remediations Themis can execute.
var KnownCapabilities = []string{"deployment.rollback-to-revision"}

// InvestigationBudget returns p's investigation bounds with defaults filled in.
func (p Profile) InvestigationBudget() Investigation {
	b := p.Investigation
	if b.MaxSteps == 0 {
		b.MaxSteps = DefaultMaxSteps
	}
	if b.MaxEvidenceRequests == 0 {
		b.MaxEvidenceRequests = DefaultMaxEvidenceRequests
	}
	if b.Timeout.Duration == 0 {
		b.Timeout.Duration = DefaultInvestigationTimeout
	}
	return b
}

// AllowsTemplate reports whether investigations of p may use template.
func (p Profile) AllowsTemplate(template string) bool {
	return slices.Contains(p.Evidence.Allowed, template)
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

// Match selects alerts. An alert matches when every condition holds.
type Match struct {
	// AlertNames lists the alertname values that match.
	AlertNames []string `json:"alertNames" jsonschema:"minItems=1,uniqueItems=true"`

	// Labels must all be present on the alert with these values.
	Labels map[string]string `json:"labels,omitempty"`

	// TargetKinds lists the resolved target kinds that match, as Kind.group.
	TargetKinds []string `json:"targetKinds" jsonschema:"minItems=1,uniqueItems=true"`
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

// Validate reports every problem in c, not only the first.
func (c Config) Validate() error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
	}

	if len(c.EnrolledNamespaces) == 0 {
		add("enrolledNamespaces", "at least one namespace is required")
	}
	seenNS := map[string]bool{}
	for i, ns := range c.EnrolledNamespaces {
		path := fmt.Sprintf("enrolledNamespaces[%d]", i)
		for _, msg := range validation.IsDNS1123Label(ns) {
			add(path, "%q: %s", ns, msg)
		}
		if seenNS[ns] {
			add(path, "duplicate namespace %q", ns)
		}
		seenNS[ns] = true
	}

	if c.Intake.BearerTokenFile == "" {
		add("intake.bearerTokenFile", "is required")
	}

	if len(c.Profiles) == 0 {
		add("profiles", "at least one profile is required")
	}
	seenName := map[string]bool{}
	seenPriority := map[int]string{}
	for i, p := range c.Profiles {
		path := fmt.Sprintf("profiles[%d]", i)
		for _, msg := range validation.IsDNS1123Label(p.Name) {
			add(path+".name", "%q: %s", p.Name, msg)
		}
		if seenName[p.Name] {
			add(path+".name", "duplicate profile name %q", p.Name)
		}
		seenName[p.Name] = true
		if p.Priority < 1 {
			add(path+".priority", "must be at least 1, got %d", p.Priority)
		} else {
			if other, ok := seenPriority[p.Priority]; ok {
				add(path+".priority", "priority %d is already used by profile %q", p.Priority, other)
			}
			seenPriority[p.Priority] = p.Name
		}

		if len(p.Match.AlertNames) == 0 {
			add(path+".match.alertNames", "at least one alert name is required")
		}
		for j, name := range p.Match.AlertNames {
			if name == "" {
				add(fmt.Sprintf("%s.match.alertNames[%d]", path, j), "must not be empty")
			}
		}
		for key, value := range p.Match.Labels {
			for _, msg := range validation.IsQualifiedName(key) {
				add(path+".match.labels", "key %q: %s", key, msg)
			}
			for _, msg := range validation.IsValidLabelValue(value) {
				add(path+".match.labels", "value %q of %q: %s", value, key, msg)
			}
		}
		if len(p.Match.TargetKinds) == 0 {
			add(path+".match.targetKinds", "at least one target kind is required")
		}
		for j, kind := range p.Match.TargetKinds {
			if !IsTargetKind(kind) {
				add(fmt.Sprintf("%s.match.targetKinds[%d]", path, j),
					"%q is not of the form Kind.group, for example Deployment.apps", kind)
			}
		}

		if len(p.Evidence.Allowed) == 0 {
			add(path+".evidence.allowed", "at least one template is required")
		}
		for j, name := range p.Evidence.Allowed {
			if slices.Contains(p.Evidence.Allowed[:j], name) {
				add(fmt.Sprintf("%s.evidence.allowed[%d]", path, j), "duplicate template %q", name)
			}
		}
		for j, name := range p.Evidence.Initial {
			if !p.AllowsTemplate(name) {
				add(fmt.Sprintf("%s.evidence.initial[%d]", path, j), "template %q is not in evidence.allowed", name)
			}
		}
		for j, name := range p.Capabilities {
			if !slices.Contains(KnownCapabilities, name) {
				add(fmt.Sprintf("%s.capabilities[%d]", path, j), "unknown capability %q", name)
			}
		}
		if p.Investigation.MaxSteps < 0 {
			add(path+".investigation.maxSteps", "must be positive")
		}
		if p.Investigation.MaxEvidenceRequests < 0 {
			add(path+".investigation.maxEvidenceRequests", "must be positive")
		}
		if p.Investigation.Timeout.Duration < 0 {
			add(path+".investigation.timeout", "must be positive")
		}
	}

	return errors.Join(errs...)
}

// IsEnrolled reports whether alerts about namespace ns may create work.
func (c Config) IsEnrolled(ns string) bool {
	return slices.Contains(c.EnrolledNamespaces, ns)
}

// TargetKind returns t as Kind.group.
func TargetKind(t incident.TargetRef) string {
	if t.Group == "" {
		return t.Kind
	}
	return t.Kind + "." + t.Group
}

// MatchProfile returns the highest-priority profile matching the alert.
func (c Config) MatchProfile(alertName string, lbls map[string]string, target incident.TargetRef) (Profile, bool) {
	candidates := make([]Profile, 0, len(c.Profiles))

	for _, profile := range c.Profiles {
		if slices.Contains(profile.Match.AlertNames, alertName) &&
			slices.Contains(profile.Match.TargetKinds, TargetKind(target)) &&
			labels.SelectorFromSet(profile.Match.Labels).Matches(labels.Set(lbls)) {
			candidates = append(candidates, profile)
		}
	}

	if len(candidates) == 0 {
		return Profile{}, false
	}

	// get the candidate with highest priority
	highestCandidate := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.Priority > highestCandidate.Priority {
			highestCandidate = candidate
		}
	}

	return highestCandidate, true
}
