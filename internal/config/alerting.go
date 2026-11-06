package config

import (
	"slices"
	"time"

	"github.com/prometheus/alertmanager/pkg/labels"
)

// Alerting says where Themis gets alerts besides its webhook.
type Alerting struct {
	// Alertmanager is the Alertmanager whose active alerts Themis reads.
	Alertmanager Alertmanager `json:"alertmanager,omitzero"`
}

// Alertmanager configures reading the active alerts from Alertmanager's API.
// Reading is on unless disabled; without an address, the Setup page sets
// one.
type Alertmanager struct {
	// Disabled stops reading: alerts arrive only through the webhook.
	Disabled bool `json:"disabled,omitempty"`

	// Address is Alertmanager's base URL, under which /api/v2/alerts is
	// served, for example http://alertmanager-operated.monitoring:9093.
	Address string `json:"address,omitempty"`

	// Found says the chart found Address in the cluster at install, rather
	// than the install setting it, so the Setup page may replace it.
	Found bool `json:"found,omitempty"`

	// BearerTokenFile holds a token sent as a bearer token, if Alertmanager
	// needs one. Read on every request, so rotation needs no restart.
	BearerTokenFile string `json:"bearerTokenFile,omitempty"`

	// BasicAuth is a username and password, if Alertmanager needs them.
	BasicAuth *BasicAuth `json:"basicAuth,omitempty"`

	// TLS configures an https address.
	TLS TLS `json:"tls,omitzero"`

	// Matchers narrow the alerts read, beyond the names the profiles answer,
	// such as cluster="prod-eu" for an Alertmanager several clusters share.
	Matchers []string `json:"matchers,omitempty"`

	// Interval is how often Alertmanager is read. Defaults to 15s.
	Interval Duration `json:"interval,omitzero"`
}

// BasicAuth is a username and a password read from a file.
type BasicAuth struct {
	Username string `json:"username" jsonschema:"minLength=1"`
	// PasswordFile is read on every request, so rotation needs no restart.
	PasswordFile string `json:"passwordFile" jsonschema:"minLength=1"`
}

// DefaultAlertmanagerInterval is how often Alertmanager is read by default.
const DefaultAlertmanagerInterval = 15 * time.Second

// EffectiveInterval returns how often Alertmanager is read.
func (a Alertmanager) EffectiveInterval() time.Duration {
	if a.Interval.Duration == 0 {
		return DefaultAlertmanagerInterval
	}
	return a.Interval.Duration
}

// Credentialed says the install gives Alertmanager's address credentials or
// TLS settings, which go only to the address the install set.
func (a Alertmanager) Credentialed() bool {
	return a.BearerTokenFile != "" || a.BasicAuth != nil || a.TLS != (TLS{})
}

// ParsedMatchers returns Matchers, parsed as Alertmanager parses them.
func (a Alertmanager) ParsedMatchers() ([]*labels.Matcher, error) {
	out := make([]*labels.Matcher, 0, len(a.Matchers))
	for _, m := range a.Matchers {
		parsed, err := labels.ParseMatcher(m)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

func (a Alertmanager) validate(add func(path, format string, args ...any)) {
	const path = "alerting.alertmanager"
	if a.Address != "" && !IsHTTPURL(a.Address) {
		add(path+".address", "%q is not an http or https URL", a.Address)
	}
	if a.Found && a.Address == "" {
		add(path+".found", "is set without an address")
	}
	// Credentials go with the address they were made for, never with one set
	// later on the Setup page.
	if a.Credentialed() && (a.Address == "" || a.Found) {
		add(path, "a token, basic auth or TLS settings need the address set with them")
	}
	if a.BearerTokenFile != "" && a.BasicAuth != nil {
		add(path, "set bearerTokenFile or basicAuth, not both")
	}
	if b := a.BasicAuth; b != nil && (b.Username == "" || b.PasswordFile == "") {
		add(path+".basicAuth", "set both username and passwordFile")
	}
	if (a.TLS.CertFile == "") != (a.TLS.KeyFile == "") {
		add(path+".tls", "set both certFile and keyFile, or neither")
	}
	for i, m := range a.Matchers {
		if _, err := labels.ParseMatcher(m); err != nil {
			add(path+".matchers", "%d: %q: %v", i, m, err)
		}
	}
	if d := a.Interval.Duration; d != 0 && d < time.Second {
		add(path+".interval", "must be at least 1s, got %s", d)
	}
}

// AlertNames are the alerts some profile answers, sorted, each once.
func (c Config) AlertNames() []string {
	var names []string
	for _, p := range c.Profiles {
		names = append(names, p.Match.AlertNames...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
