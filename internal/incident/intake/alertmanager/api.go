package alertmanager

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/fetch"
)

const (
	// requestTimeout bounds each request to Alertmanager, below the shortest
	// sensible interval between reads.
	requestTimeout = 10 * time.Second
	// maxAlertsBytes bounds the list of alerts: thousands, with their labels
	// and annotations.
	maxAlertsBytes = 16 << 20
	// maxStatusBytes bounds Alertmanager's status, which holds its whole
	// configuration.
	maxStatusBytes = 4 << 20
)

// apiAlert is an alert as Alertmanager's API v2 lists it.
type apiAlert struct {
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Status       struct {
		// State is active, suppressed (silenced or inhibited) or unprocessed.
		State string `json:"state"`
	} `json:"status"`
}

// stateActive is the state of an alert neither silenced nor inhibited.
const stateActive = "active"

// apiStatus is what Alertmanager's API v2 says about itself.
type apiStatus struct {
	// Uptime is when this Alertmanager started.
	Uptime      time.Time `json:"uptime"`
	VersionInfo struct {
		Version string `json:"version"`
	} `json:"versionInfo"`
}

// client reads one Alertmanager's API v2.
type client struct {
	*fetch.Client
}

// newClient reads Alertmanager at address. With credentials, it sends the
// install's token or basic auth and checks TLS as the install says; only the
// address the install set gets those.
func newClient(install config.Alertmanager, address string, credentials bool) (*client, error) {
	var creds fetch.Credentials
	var tlsConfig *tls.Config
	if credentials {
		var err error
		if tlsConfig, err = install.TLS.ClientConfig(); err != nil {
			return nil, fmt.Errorf("alerting.alertmanager.tls: %w", err)
		}
		creds.BearerTokenFile = install.BearerTokenFile
		if b := install.BasicAuth; b != nil {
			creds.Username, creds.PasswordFile = b.Username, b.PasswordFile
		}
	}
	c, err := fetch.New("Alertmanager", address, creds, tlsConfig, requestTimeout)
	if err != nil {
		return nil, err
	}
	return &client{c}, nil
}

// alerts lists the alerts that match every one of filter, silenced and
// inhibited ones included. Alertmanager leaves out those that ended.
func (c *client) alerts(ctx context.Context, filter []string) ([]apiAlert, error) {
	q := url.Values{"active": {"true"}, "silenced": {"true"}, "inhibited": {"true"}}
	for _, f := range filter {
		q.Add("filter", f)
	}
	var out []apiAlert
	return out, c.Get(ctx, "api/v2/alerts", q, maxAlertsBytes, &out)
}

// status says when Alertmanager started, and its version.
func (c *client) status(ctx context.Context) (apiStatus, error) {
	var out apiStatus
	return out, c.Get(ctx, "api/v2/status", nil, maxStatusBytes, &out)
}
