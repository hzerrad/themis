package alertmanager

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/store"
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

// readError says why Alertmanager couldn't be read, for people, and of
// which kind, for the remedy.
type readError struct {
	kind, msg string
}

func (e *readError) Error() string { return e.msg }

// failureKind is the kind of a failed read: store.ReadUnreachable,
// store.ReadRefused or store.ReadUnexpected.
func failureKind(err error) string {
	if re, ok := errors.AsType[*readError](err); ok {
		return re.kind
	}
	return store.ReadUnexpected
}

// client reads one Alertmanager's API v2.
type client struct {
	base  *url.URL
	shown string // the address, with any password hidden
	// tokenFile and basic are its credentials, if any.
	tokenFile string
	basic     *config.BasicAuth
	http      *http.Client
}

// newClient reads Alertmanager at address. With credentials, it sends the
// install's token or basic auth and checks TLS as the install says; only the
// address the install set gets those.
func newClient(install config.Alertmanager, address string, credentials bool) (*client, error) {
	base, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("alertmanager's address: %w", err)
	}
	c := &client{base: base, shown: base.Redacted(), http: &http.Client{Timeout: requestTimeout}}
	if credentials {
		tlsConfig, err := install.TLS.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("alerting.alertmanager.tls: %w", err)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsConfig
		c.http.Transport = transport
		c.tokenFile, c.basic = install.BearerTokenFile, install.BasicAuth
	}
	return c, nil
}

// alerts lists the alerts that match every one of filter, silenced and
// inhibited ones included. Alertmanager leaves out those that ended.
func (c *client) alerts(ctx context.Context, filter []string) ([]apiAlert, error) {
	q := url.Values{"active": {"true"}, "silenced": {"true"}, "inhibited": {"true"}}
	for _, f := range filter {
		q.Add("filter", f)
	}
	var out []apiAlert
	return out, c.get(ctx, "alerts", q, maxAlertsBytes, &out)
}

// status says when Alertmanager started, and its version.
func (c *client) status(ctx context.Context) (apiStatus, error) {
	var out apiStatus
	return out, c.get(ctx, "status", nil, maxStatusBytes, &out)
}

func (c *client) get(ctx context.Context, path string, query url.Values, limit int64, into any) error {
	u := c.base.JoinPath("api", "v2", path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case c.tokenFile != "":
		secret, err := token.Read(c.tokenFile)
		if err != nil {
			return fmt.Errorf("alertmanager's token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+string(secret))
	case c.basic != nil:
		password, err := token.Read(c.basic.PasswordFile)
		if err != nil {
			return fmt.Errorf("alertmanager's password: %w", err)
		}
		req.SetBasicAuth(c.basic.Username, string(password))
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ue, ok := errors.AsType[*url.Error](err); ok {
			if ue.Timeout() {
				return &readError{store.ReadUnreachable, fmt.Sprintf("alertmanager at %s didn't answer within %s", c.shown, requestTimeout)}
			}
			err = ue.Err
		}
		return &readError{store.ReadUnreachable, fmt.Sprintf("alertmanager at %s doesn't answer: %v", c.shown, err)}
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return &readError{store.ReadRefused, fmt.Sprintf("alertmanager at %s answered %s", c.shown, res.Status)}
	case res.StatusCode != http.StatusOK:
		return &readError{store.ReadUnexpected, fmt.Sprintf("alertmanager at %s answered %s", c.shown, res.Status)}
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return &readError{store.ReadUnreachable, fmt.Sprintf("alertmanager at %s stopped answering: %v", c.shown, err)}
	}
	if int64(len(body)) > limit {
		return &readError{store.ReadUnexpected, fmt.Sprintf("alertmanager at %s answered with more than %d MiB", c.shown, limit>>20)}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return &readError{store.ReadUnexpected, fmt.Sprintf("the answer from %s isn't Alertmanager's: %v", c.shown, err)}
	}
	return nil
}
