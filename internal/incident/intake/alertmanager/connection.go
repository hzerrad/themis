package alertmanager

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/store"
)

// Addresses keeps the address of the Alertmanager Themis reads.
type Addresses interface {
	Alertmanager(ctx context.Context) (settings.AlertmanagerView, error)
	SetAlertmanager(ctx context.Context, address, by string) error
}

// Reads says how Alertmanager was last read.
type Reads interface {
	LastAlertRead(ctx context.Context, source string) (store.AlertRead, error)
}

// Connection is the Alertmanager Themis reads: what the install set, the
// address in use, which the Setup page may set, and how the last read went.
type Connection struct {
	Install   config.Alertmanager
	Addresses Addresses
	Reads     Reads

	mu        sync.Mutex
	client    *client
	clientFor string
}

// View returns the address in use, and where it comes from.
func (c *Connection) View(ctx context.Context) (settings.AlertmanagerView, error) {
	return c.Addresses.Alertmanager(ctx)
}

// SetAddress saves the address the Setup page gives, set by by.
func (c *Connection) SetAddress(ctx context.Context, address, by string) error {
	return c.Addresses.SetAlertmanager(ctx, address, by)
}

// LastRead says how Alertmanager was last read, or store.ErrNotFound if
// never.
func (c *Connection) LastRead(ctx context.Context) (store.AlertRead, error) {
	return c.Reads.LastAlertRead(ctx, Source)
}

// reader returns a client for the address in use, or nil without one.
func (c *Connection) reader(ctx context.Context) (*client, settings.AlertmanagerView, error) {
	v, err := c.View(ctx)
	if err != nil || v.Address == "" || !v.Reading {
		return nil, v, err
	}
	credentials := v.From == settings.FromInstall
	key := v.Address
	if credentials {
		key += " with the install's credentials"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.clientFor != key {
		cl, err := newClient(c.Install, v.Address, credentials)
		if err != nil {
			return nil, v, err
		}
		c.client, c.clientFor = cl, key
	}
	return c.client, v, nil
}

// TestResult is what Alertmanager lists now.
type TestResult struct {
	Address string `json:"address"`
	Version string `json:"version,omitempty"`
	// Active counts the alerts neither silenced nor inhibited.
	Active int `json:"active"`
	// Firing are the alerts Themis answers that are active, by name.
	Firing []Firing `json:"firing"`
}

// Firing counts the active alerts of one name.
type Firing struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ErrNoAddress means there is no address to test.
var ErrNoAddress = errors.New("no address to test. Enter Alertmanager's address")

// Test reads Alertmanager at address, or at the address in use when address
// is empty, and says which of the answered alerts fire now. An address other
// than the install's is read without its credentials.
func (c *Connection) Test(ctx context.Context, address string, answered []string) (TestResult, error) {
	credentials := false
	if address == "" {
		v, err := c.View(ctx)
		if err != nil {
			return TestResult{}, err
		}
		address, credentials = v.Address, v.From == settings.FromInstall
	}
	if address == "" {
		return TestResult{}, ErrNoAddress
	}
	if !config.IsHTTPURL(address) {
		return TestResult{}, settings.ErrNotHTTPURL
	}
	matchers, err := c.Install.ParsedMatchers()
	if err != nil {
		return TestResult{}, err
	}
	cl, err := newClient(c.Install, address, credentials)
	if err != nil {
		return TestResult{}, err
	}
	st, err := cl.status(ctx)
	if err != nil {
		return TestResult{}, err
	}
	list, err := cl.alerts(ctx, nil)
	if err != nil {
		return TestResult{}, err
	}
	out := TestResult{Address: cl.shown, Version: st.VersionInfo.Version, Firing: []Firing{}}
	counts := map[string]int{}
	for _, a := range list {
		if a.Status.State != stateActive {
			continue
		}
		out.Active++
		name := a.Labels["alertname"]
		if !slices.Contains(answered, name) {
			continue
		}
		matches := true
		for _, m := range matchers {
			matches = matches && m.Matches(a.Labels[m.Name])
		}
		if matches {
			counts[name]++
		}
	}
	for _, name := range answered {
		if counts[name] > 0 {
			out.Firing = append(out.Firing, Firing{Name: name, Count: counts[name]})
		}
	}
	return out, nil
}
