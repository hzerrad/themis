package config

import (
	"maps"
	"slices"
	"time"
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

	// Priority orders the profiles matching the same alert, highest first:
	// the first is the incident's primary profile. Every one that matches
	// applies. Unique, from 1.
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

	// JustifiedBy names, for each fix in Capabilities, the checks that
	// justify it: Themis refuses the fix unless one of them is true for the
	// incident, whoever suggests it. Each must be a check the profile runs.
	JustifiedBy map[string][]string `json:"justifiedBy,omitempty"`

	// Symptoms are the checks that find the failure itself, such as OOM
	// kills or throttling at the limit. While one is true and no fix has been
	// applied, Themis never closes the incident with no change on its own: it
	// refers it to a person and waits for a suggested fix. None means the
	// checks in JustifiedBy. Each must be a check the profile runs.
	Symptoms []string `json:"symptoms,omitempty" jsonschema:"uniqueItems=true"`

	// Guidance tells the investigator, in plain words, when this profile's
	// fixes are right and when they are wrong. It guides; checks and policy
	// decide.
	Guidance string `json:"guidance,omitempty" jsonschema:"maxLength=4000"`

	// Investigation bounds each investigation; zero fields take defaults.
	Investigation Investigation `json:"investigation,omitzero"`

	// Policy decides whether proposed actions run, wait for approval, or not.
	Policy Policy `json:"policy,omitzero"`

	// Verification measures an action's outcome; zero fields take defaults.
	Verification Verification `json:"verification,omitzero"`
}

// EffectiveSymptoms are the checks that find p's failure: its symptoms, or
// without any, the checks that justify its fixes.
func (p Profile) EffectiveSymptoms() []string {
	if len(p.Symptoms) > 0 {
		return p.Symptoms
	}
	var out []string
	for _, fix := range slices.Sorted(maps.Keys(p.JustifiedBy)) {
		out = union(out, p.JustifiedBy[fix])
	}
	return out
}

// Verification bounds how an action's outcome is measured.
type Verification struct {
	// Window is how long to watch the target after an action.
	Window Duration `json:"window,omitzero"`
	// Stability is how long the target must stay healthy within the window.
	Stability Duration `json:"stability,omitzero"`
	// Checks are the checks that find the failure the profile's fixes are
	// meant to cure, such as throttling at the CPU limit: a fix counts as
	// fixed only when each is false, decided again from what the workload
	// did since the change. None means its pods' readiness, restarts and OOM
	// kills alone. Each must be a check the profile runs.
	Checks []string `json:"checks,omitempty" jsonschema:"uniqueItems=true"`
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

// MaxGuidance is the most characters a profile's guidance may have: enough
// for a few paragraphs, little enough to leave the investigator room for the
// evidence.
const MaxGuidance = 4000

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

// Profile returns the profile named name.
func (c Config) Profile(name string) (Profile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}
