// Package config loads the Themis configuration file.
//
// The file is shaped like the custom resources that will replace it: keys are
// camelCase and fields use JSON tags, so moving to CRDs changes where the
// configuration comes from, not its structure.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/incident"
)

// TargetKindPattern is the syntax of a target kind: an UpperCamelCase Kind,
// then a dot and the API group, which is omitted for the core group. For
// example Deployment.apps, Rollout.argoproj.io or PersistentVolumeClaim.
const TargetKindPattern = `^[A-Z][A-Za-z0-9]*(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)?$`

var targetKindRE = regexp.MustCompile(TargetKindPattern)

// IsTargetKind reports whether s is a well-formed Kind.group.
func IsTargetKind(s string) bool {
	return targetKindRE.MatchString(s)
}

// Config is the root of the configuration file.
type Config struct {
	// EnrolledNamespaces are the namespaces Themis watches. Alerts about any
	// other namespace are recorded as rejected and never create work. Each must
	// also be enrolled in the Helm values, which grant the RBAC to read it.
	EnrolledNamespaces []string `json:"enrolledNamespaces" jsonschema:"minItems=1,uniqueItems=true"`

	// Intake configures how alerts reach Themis.
	Intake Intake `json:"intake"`

	// Profiles classify incoming alerts into incident classes. When several
	// profiles match an alert, the one with the highest priority wins.
	Profiles []Profile `json:"profiles" jsonschema:"minItems=1"`
}

// Intake configures the Alertmanager webhook.
type Intake struct {
	// BearerTokenFile is the path to a file holding the token Alertmanager must
	// send in its Authorization header. In a cluster it is a mounted Secret.
	BearerTokenFile string `json:"bearerTokenFile" jsonschema:"minLength=1"`
}

// Profile is one incident class and the alerts that belong to it.
type Profile struct {
	// Name identifies the profile, for example rollout-regression. It must be a
	// valid DNS label.
	Name string `json:"name" jsonschema:"minLength=1,maxLength=63"`

	// Priority orders profiles that match the same alert; higher wins.
	// Priorities must be unique, so the choice is always deterministic.
	Priority int `json:"priority"`

	// Match decides which alerts belong to this profile.
	Match Match `json:"match"`
}

// Match selects alerts. An alert matches when every condition holds.
type Match struct {
	// AlertNames lists the alertname values that match, for example
	// KubePodCrashLooping.
	AlertNames []string `json:"alertNames" jsonschema:"minItems=1,uniqueItems=true"`

	// Labels must all be present on the alert with exactly these values.
	// Other labels on the alert are ignored.
	Labels map[string]string `json:"labels,omitempty"`

	// TargetKinds lists the resolved target kinds that match, written as
	// Kind.group, for example Deployment.apps or StatefulSet.apps. Kinds in
	// the core API group have no suffix, for example PersistentVolumeClaim.
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

// Parse parses and validates a configuration document. Unknown and duplicate
// keys are errors, so a misspelled field is reported instead of ignored.
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
		if other, ok := seenPriority[p.Priority]; ok {
			add(path+".priority", "priority %d is already used by profile %q", p.Priority, other)
		}
		seenPriority[p.Priority] = p.Name

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
	}

	return errors.Join(errs...)
}

// IsEnrolled reports whether alerts about namespace ns may create work.
func (c Config) IsEnrolled(ns string) bool {
	return slices.Contains(c.EnrolledNamespaces, ns)
}

// TargetKind returns the Kind.group form of t used in TargetKinds. Kinds in
// the core API group have no suffix, as in kubectl.
func TargetKind(t incident.TargetRef) string {
	if t.Group == "" {
		return t.Kind
	}
	return t.Kind + "." + t.Group
}

// MatchProfile returns the highest-priority profile that matches an alert with
// the given name and labels, resolved to target. It returns false when no
// profile matches.
func (c Config) MatchProfile(alertName string, labels map[string]string, target incident.TargetRef) (Profile, bool) {
	// TODO(you): implement. See the tests in config_test.go.
	return Profile{}, false
}
