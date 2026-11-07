package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ExternalProposals configures fixes suggested from outside Themis. They pass
// the same checks as Themis's own.
type ExternalProposals struct {
	// Sources are the agents allowed to suggest fixes.
	Sources []ProposalSource `json:"sources,omitempty"`

	// Window is how long an incident Themis couldn't decide stays open for a
	// suggestion, and how long one waiting for a person's fix waits before
	// Themis looks again: it keeps waiting while its checks find the failure.
	// Defaults to 15m with sources, and to none without.
	Window Duration `json:"window,omitzero"`
}

// ProposalSource is one agent, known by its Kubernetes ServiceAccount.
type ProposalSource struct {
	// ServiceAccount is its identity, as namespace/name.
	ServiceAccount string `json:"serviceAccount" jsonschema:"minLength=3"`

	// Namespaces are those whose incidents it may read and suggest fixes for.
	Namespaces []string `json:"namespaces" jsonschema:"minItems=1,uniqueItems=true"`

	// Capabilities are the fixes it may suggest.
	Capabilities []string `json:"capabilities" jsonschema:"minItems=1,uniqueItems=true"`

	// PerMinute bounds its requests to suggest a fix. Defaults to 6.
	PerMinute int `json:"perMinute,omitempty" jsonschema:"minimum=1"`

	// Ceiling is the most sensitive evidence it may read: internal, or
	// sensitive to include container logs. Defaults to internal.
	Ceiling string `json:"ceiling,omitempty" jsonschema:"enum=internal,enum=sensitive"`
}

// External proposal defaults.
const (
	DefaultProposalWindow = 15 * time.Minute
	DefaultPerMinute      = 6
)

// EffectiveWindow returns how long an undecided incident stays open.
func (e ExternalProposals) EffectiveWindow() time.Duration {
	if e.Window.Duration == 0 && len(e.Sources) > 0 {
		return DefaultProposalWindow
	}
	return e.Window.Duration
}

// EffectivePerMinute returns how many requests to suggest a fix the source
// may make each minute.
func (p ProposalSource) EffectivePerMinute() int {
	if p.PerMinute == 0 {
		return DefaultPerMinute
	}
	return p.PerMinute
}

// SuggestionWindow returns how long an incident waits for a suggestion: after
// Themis couldn't decide, or, without an investigator, from when it opens.
// Without an investigator only suggestions can fix anything, so it's never
// none: people need time to answer.
func (c Config) SuggestionWindow() time.Duration {
	if c.Investigators.Disabled {
		return c.AwaitingSuggestionWindow()
	}
	return c.ExternalProposals.EffectiveWindow()
}

// AwaitingSuggestionWindow returns how long an incident waits for a person
// to suggest a fix before Themis looks at it again: without an investigator,
// from when it opens, and after a person denied its fix, from the denial.
// One whose checks still find its failure then keeps waiting. People need
// time to answer, so it's never none.
func (c Config) AwaitingSuggestionWindow() time.Duration {
	if w := c.ExternalProposals.EffectiveWindow(); w > 0 {
		return w
	}
	return DefaultProposalWindow
}

// Source returns the source with the given ServiceAccount.
func (e ExternalProposals) Source(serviceAccount string) (ProposalSource, bool) {
	for _, s := range e.Sources {
		if s.ServiceAccount == serviceAccount {
			return s, true
		}
	}
	return ProposalSource{}, false
}

// Validate reports every problem in e, not only the first.
func (e ExternalProposals) Validate() error {
	var errs []error
	e.validate(func(path, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
	})
	return errors.Join(errs...)
}

func (e ExternalProposals) validate(add func(path, format string, args ...any)) {
	if e.Window.Duration < 0 {
		add("window", "must be positive")
	}
	seen := map[string]bool{}
	for i, s := range e.Sources {
		path := fmt.Sprintf("sources[%d]", i)
		ns, name, ok := strings.Cut(s.ServiceAccount, "/")
		if !ok || len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1123Subdomain(name)) > 0 {
			add(path+".serviceAccount", "%q is not of the form namespace/name", s.ServiceAccount)
		}
		if seen[s.ServiceAccount] {
			add(path+".serviceAccount", "duplicate source %q", s.ServiceAccount)
		}
		seen[s.ServiceAccount] = true
		if len(s.Namespaces) == 0 {
			add(path+".namespaces", "at least one namespace is required")
		}
		if len(s.Capabilities) == 0 {
			add(path+".capabilities", "at least one capability is required")
		}
		if s.PerMinute < 0 {
			add(path+".perMinute", "must be positive")
		}
		if s.Ceiling != "" && s.Ceiling != "internal" && s.Ceiling != "sensitive" {
			add(path+".ceiling", "must be internal or sensitive, got %q", s.Ceiling)
		}
	}
}
