// Package fetch reads JSON from the HTTP servers Themis is set up to read,
// such as Alertmanager and Prometheus, and says why one couldn't be read in
// words people can act on.
package fetch

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hzerrad/themis/internal/platform/token"
)

// Why a server couldn't be read.
const (
	// Unreachable: it didn't answer.
	Unreachable = "unreachable"
	// Refused: it refused Themis's credentials, or asked for some.
	Refused = "refused"
	// Unexpected: it answered, but not as it should.
	Unexpected = "unexpected"
)

// Error says why a server couldn't be read, and of which kind.
type Error struct {
	Kind string
	msg  string
}

func (e *Error) Error() string { return e.msg }

// KindOf returns the kind of a failed read: Unexpected for an error that
// isn't an Error.
func KindOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	return Unexpected
}

// Credentials prove who Themis is: a bearer token, or a username and a
// password. Files are read on every request, so rotation needs no restart.
type Credentials struct {
	BearerTokenFile string
	Username        string
	PasswordFile    string
}

// Client reads one server.
type Client struct {
	name  string
	base  *url.URL
	shown string
	creds Credentials
	http  *http.Client
}

// New reads the server called name, such as alertmanager, at address, with
// creds, checking its certificate as tlsConfig says, and waiting at most
// timeout for each answer. A nil tlsConfig uses the system's.
func New(name, address string, creds Credentials, tlsConfig *tls.Config, timeout time.Duration) (*Client, error) {
	base, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("%s's address: %w", name, err)
	}
	c := &Client{name: name, base: base, shown: base.Redacted(), creds: creds, http: &http.Client{Timeout: timeout}}
	if tlsConfig != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsConfig
		c.http.Transport = transport
	}
	return c, nil
}

// Address is the server's address, with any password hidden.
func (c *Client) Address() string { return c.shown }

// Get reads path under the address, with query, into into, from an answer of
// at most limit bytes.
func (c *Client) Get(ctx context.Context, path string, query url.Values, limit int64, into any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case c.creds.BearerTokenFile != "":
		secret, err := token.Read(c.creds.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("%s's token: %w", c.name, err)
		}
		req.Header.Set("Authorization", "Bearer "+string(secret))
	case c.creds.PasswordFile != "":
		password, err := token.Read(c.creds.PasswordFile)
		if err != nil {
			return fmt.Errorf("%s's password: %w", c.name, err)
		}
		req.SetBasicAuth(c.creds.Username, string(password))
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ue, ok := errors.AsType[*url.Error](err); ok {
			if ue.Timeout() {
				return c.fail(Unreachable, "didn't answer within %s", c.http.Timeout)
			}
			err = ue.Err
		}
		return c.fail(Unreachable, "doesn't answer: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return c.fail(Refused, "answered %s", res.Status)
	case res.StatusCode != http.StatusOK:
		return c.fail(Unexpected, "answered %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return c.fail(Unreachable, "stopped answering: %v", err)
	}
	if int64(len(body)) > limit {
		return c.fail(Unexpected, "answered with more than %d MiB", limit>>20)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return &Error{Unexpected, fmt.Sprintf("the answer from %s isn't %s's: %v", c.shown, c.name, err)}
	}
	return nil
}

// fail says what the server did, after its name and address.
func (c *Client) fail(kind, format string, args ...any) error {
	return &Error{kind, fmt.Sprintf("%s at %s ", c.name, c.shown) + fmt.Sprintf(format, args...)}
}
