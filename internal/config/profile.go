package config

import (
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/hzerrad/themis/internal/incident"
)

// Scenarios configures the scenario packs. Every pack is on unless disabled.
type Scenarios struct {
	// Disabled names the packs to turn off: their profiles and checks are
	// left out.
	Disabled []string `json:"disabled,omitempty" jsonschema:"uniqueItems=true"`
}

// Profile is one incident class and the alerts that belong to it.
type Profile struct {
	// Name is a DNS label, for example rollout-regression.
	Name string `json:"name" jsonschema:"minLength=1,maxLength=63"`

	// Scope is the only namespace the profile applies to; empty means every
	// enrolled one. Only a resource in a team's namespace sets it.
	Scope string `json:"-"`

	// Priority orders profiles matching the same alert; higher wins. Unique,
	// from 1.
	Priority int `json:"priority" jsonschema:"minimum=1"`

	// Match decides which alerts belong to this profile.
	Match Match `json:"match"`

	// Evidence lists the templates investigations of this profile may use.
	Evidence Evidence `json:"evidence"`

	// Checks are the checks this profile runs, by name; none means every
	// check Themis has.
	Checks []string `json:"checks,omitempty" jsonschema:"uniqueItems=true"`

	// Capabilities are the remediations proposals may name; none means
	// diagnosis only.
	Capabilities []string `json:"capabilities,omitempty" jsonschema:"uniqueItems=true"`

	// Investigation bounds each investigation; zero fields take defaults.
	Investigation Investigation `json:"investigation,omitzero"`

	// Policy decides whether proposed actions run, wait for approval, or not.
	Policy Policy `json:"policy,omitzero"`

	// Verification measures an action's outcome; zero fields take defaults.
	Verification Verification `json:"verification,omitzero"`
}

// Verification bounds how an action's outcome is measured.
type Verification struct {
	// Window is how long to watch the target after an action.
	Window Duration `json:"window,omitzero"`
	// Stability is how long the target must stay healthy within the window.
	Stability Duration `json:"stability,omitzero"`
}

// Policy and verification defaults.
const (
	DefaultApprovalTimeout = 30 * time.Minute
	DefaultVerifyWindow    = 5 * time.Minute
	DefaultVerifyStability = 2 * time.Minute
)

// EffectiveVerification returns p's verification with defaults filled in.
func (p Profile) EffectiveVerification() Verification {
	v := p.Verification
	if v.Window.Duration == 0 {
		v.Window.Duration = DefaultVerifyWindow
	}
	if v.Stability.Duration == 0 {
		v.Stability.Duration = min(DefaultVerifyStability, v.Window.Duration)
	}
	return v
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

// Match selects alerts. An alert matches when every condition holds.
type Match struct {
	// AlertNames lists the alertname values that match.
	AlertNames []string `json:"alertNames" jsonschema:"minItems=1,uniqueItems=true"`

	// Labels must all be present on the alert with these values.
	Labels map[string]string `json:"labels,omitempty"`

	// TargetKinds lists the resolved target kinds that match, as Kind.group.
	TargetKinds []string `json:"targetKinds" jsonschema:"minItems=1,uniqueItems=true"`
}

// MatchProfile returns the highest-priority profile matching the alert.
func (c Config) MatchProfile(alertName string, lbls map[string]string, target incident.TargetRef) (Profile, bool) {
	candidates := make([]Profile, 0, len(c.Profiles))

	for _, profile := range c.Profiles {
		if (profile.Scope == "" || profile.Scope == target.Namespace) &&
			slices.Contains(profile.Match.AlertNames, alertName) &&
			slices.Contains(profile.Match.TargetKinds, target.KindGroup()) &&
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
		// On a tie, the team's own profile is the more specific.
		if candidate.Priority > highestCandidate.Priority ||
			(candidate.Priority == highestCandidate.Priority && candidate.Scope != "" && highestCandidate.Scope == "") {
			highestCandidate = candidate
		}
	}

	return highestCandidate, true
}

// Profile returns the profile named name.
func (c Config) Profile(name string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}
