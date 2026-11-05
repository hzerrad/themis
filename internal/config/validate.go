package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/hzerrad/themis/internal/incident"
)

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
	if c.Investigators.BearerTokenFile == "" {
		add("investigators.bearerTokenFile", "is required")
	}
	if c.API.BearerTokenFile == "" {
		add("api.bearerTokenFile", "is required")
	}
	if o := c.API.OIDC; o != nil {
		if !IsHTTPURL(o.IssuerURL) {
			add("api.oidc.issuerURL", "%q is not an http or https URL", o.IssuerURL)
		}
		if !IsHTTPURL(o.RedirectURL) {
			add("api.oidc.redirectURL", "%q is not an http or https URL", o.RedirectURL)
		}
		if o.ClientID == "" {
			add("api.oidc.clientID", "is required")
		}
		if o.ClientSecretFile == "" {
			add("api.oidc.clientSecretFile", "is required")
		}
		if o.SessionTTL.Duration < 0 {
			add("api.oidc.sessionTTL", "must not be negative")
		}
	}
	for i, b := range c.API.Access {
		path := fmt.Sprintf("api.access[%d]", i)
		if !slices.Contains(Roles, b.Role) {
			add(path+".role", "must be one of %s, got %q", strings.Join(Roles, ", "), b.Role)
		}
		if len(b.Users) == 0 && len(b.Groups) == 0 {
			add(path, "name at least one user or group")
		}
	}
	if w := c.Notifications.Webhook; w != nil {
		switch {
		case (w.URL == "") == (w.URLFile == ""):
			add("notifications.webhook", "set exactly one of url and urlFile")
		case w.URL != "" && !IsHTTPURL(w.URL):
			add("notifications.webhook.url", "%q is not an http or https URL", w.URL)
		}
	}
	if e := c.Notifications.ExternalURL; e != "" && !IsHTTPURL(e) {
		add("notifications.externalURL", "%q is not an http or https URL", e)
	}
	if c.Executor != nil {
		if c.Executor.Address == "" {
			add("executor.address", "is required")
		}
		if c.Executor.BearerTokenFile == "" {
			add("executor.bearerTokenFile", "is required")
		}
	}

	c.Metrics.validate(add)
	c.ExternalProposals.validate(func(sub, format string, args ...any) { add("externalProposals."+sub, format, args...) })
	switch {
	case c.Resources != nil && c.Resources.Namespace == "":
		add("resources.namespace", "is required")
	case c.Resources != nil && len(c.Profiles) > 0:
		add("profiles", "set profiles as IncidentProfile resources, or remove resources")
	}
	seenName := map[string]bool{}
	seenPriority := map[int]string{}
	for i, p := range c.Profiles {
		path := fmt.Sprintf("profiles[%d]", i)
		if seenName[p.Name] {
			add(path+".name", "duplicate profile name %q", p.Name)
		}
		seenName[p.Name] = true
		if other, ok := seenPriority[p.Priority]; ok && p.Priority >= 1 {
			add(path+".priority", "priority %d is already used by profile %q", p.Priority, other)
		} else {
			seenPriority[p.Priority] = p.Name
		}
		p.validate(func(sub, format string, args ...any) { add(path+"."+sub, format, args...) })
	}

	return errors.Join(errs...)
}

// Validate reports every problem in p, not only the first.
func (p Profile) Validate() error {
	var errs []error
	p.validate(func(path, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
	})
	return errors.Join(errs...)
}

func (p Profile) validate(add func(path, format string, args ...any)) {
	for _, msg := range validation.IsDNS1123Label(p.Name) {
		add("name", "%q: %s", p.Name, msg)
	}
	if p.Priority < 1 {
		add("priority", "must be at least 1, got %d", p.Priority)
	}

	if len(p.Match.AlertNames) == 0 {
		add("match.alertNames", "at least one alert name is required")
	}
	for j, name := range p.Match.AlertNames {
		if name == "" {
			add(fmt.Sprintf("match.alertNames[%d]", j), "must not be empty")
		}
	}
	for key, value := range p.Match.Labels {
		for _, msg := range validation.IsQualifiedName(key) {
			add("match.labels", "key %q: %s", key, msg)
		}
		for _, msg := range validation.IsValidLabelValue(value) {
			add("match.labels", "value %q of %q: %s", value, key, msg)
		}
	}
	if len(p.Match.TargetKinds) == 0 {
		add("match.targetKinds", "at least one target kind is required")
	}
	for j, kind := range p.Match.TargetKinds {
		if !incident.IsKindGroup(kind) {
			add(fmt.Sprintf("match.targetKinds[%d]", j),
				"%q is not of the form Kind.group, for example Deployment.apps", kind)
		}
	}

	if len(p.Evidence.Allowed) == 0 {
		add("evidence.allowed", "at least one template is required")
	}
	for j, name := range p.Evidence.Allowed {
		if slices.Contains(p.Evidence.Allowed[:j], name) {
			add(fmt.Sprintf("evidence.allowed[%d]", j), "duplicate template %q", name)
		}
	}
	for j, name := range p.Evidence.Initial {
		if !p.AllowsTemplate(name) {
			add(fmt.Sprintf("evidence.initial[%d]", j), "template %q is not in evidence.allowed", name)
		}
	}
	if n := utf8.RuneCountInString(p.Guidance); n > MaxGuidance {
		add("guidance", "must be at most %d characters, got %d", MaxGuidance, n)
	}
	if p.Investigation.MaxSteps < 0 {
		add("investigation.maxSteps", "must be positive")
	}
	if p.Investigation.MaxEvidenceRequests < 0 {
		add("investigation.maxEvidenceRequests", "must be positive")
	}
	if p.Investigation.Timeout.Duration < 0 {
		add("investigation.timeout", "must be positive")
	}
	if m := p.Policy.Mode; m != "" && m != ModeGuarded && m != ModeObserve {
		add("policy.mode", "must be guarded or observe, got %q", m)
	}
	if p.Policy.ApprovalTimeout.Duration < 0 {
		add("policy.approvalTimeout", "must be positive")
	}
	for j, r := range p.Policy.Rules {
		rpath := fmt.Sprintf("policy.rules[%d]", j)
		if r.Name == "" {
			add(rpath+".name", "is required")
		}
		if r.When == "" {
			add(rpath+".when", "is required")
		}
		if r.Decision != DecisionAllow && r.Decision != DecisionDeny && r.Decision != DecisionRequireApproval {
			add(rpath+".decision", "must be allow, deny or require_approval, got %q", r.Decision)
		}
	}
	if v := p.EffectiveVerification(); v.Window.Duration < 0 || v.Stability.Duration < 0 || v.Stability.Duration > v.Window.Duration {
		add("verification", "stability must be positive and within the window")
	}
}

// IsHTTPURL reports whether s is an absolute http or https URL.
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
