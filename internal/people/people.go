// Package people says who is calling the HTTP API and what they may do:
// someone signed in through OIDC, with the roles their bindings give them, the
// holder of the shared admin token, or, until setup finishes, whoever reached
// Themis through kubectl port-forward.
package people

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/store"
)

// SessionCookie holds a signed-in person's session ID.
const SessionCookie = "themis_session"

// RequestHeader must accompany every change made with a session cookie. A
// page on another site can't send it without the browser asking Themis
// first, so a forged form or link can't approve a fix.
const RequestHeader = "X-Themis-Request"

// TokenUser is the username of whoever holds the admin token.
const TokenUser = "admin"

// Identity is who made a request.
type Identity struct {
	Username string
	Groups   []string
	// Role is the most trusted role their bindings give, or empty for none.
	Role string
	// Token is set for the shared admin token: it proves the token, not who.
	Token bool
	// Setup is set for whoever came in through kubectl port-forward before
	// setup finished: Kubernetes allowed them, but Themis doesn't know who.
	Setup bool
}

// Can reports whether i has role, or one more trusted.
func (i Identity) Can(role string) bool {
	return i.Role != "" && slices.Index(config.Roles, i.Role) >= slices.Index(config.Roles, role)
}

// RoleOf is the most trusted role bindings give username or any of groups.
func RoleOf(bindings []config.AccessBinding, username string, groups []string) string {
	best := -1
	for _, b := range bindings {
		matches := slices.Contains(b.Users, username) || slices.ContainsFunc(b.Groups, func(g string) bool { return slices.Contains(groups, g) })
		if r := slices.Index(config.Roles, b.Role); matches && r > best {
			best = r
		}
	}
	if best < 0 {
		return ""
	}
	return config.Roles[best]
}

// Errors from Identify.
var (
	// ErrSignedOut means the request proves no identity.
	ErrSignedOut = errors.New("signed out")
	// ErrUnavailable means Themis can't check right now.
	ErrUnavailable = errors.New("cannot check sign-ins")
	// ErrNoRequestHeader means a change was sent with a cookie but without
	// RequestHeader.
	ErrNoRequestHeader = errors.New("missing request header")
)

// Sessions keeps signed-in people.
type Sessions interface {
	Session(ctx context.Context, id string, now time.Time) (store.Session, error)
}

// Directory identifies the callers of the HTTP API.
type Directory struct {
	TokenFile string
	Access    []config.AccessBinding
	// Sessions is nil when no one signs in through OIDC.
	Sessions Sessions
	Now      func() time.Time
}

// Identify says who sent r. A bearer token must be the admin token; without
// one, the session cookie decides, and without a session, the setup door.
func (d *Directory) Identify(r *http.Request) (Identity, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		want, err := auth.ReadToken(d.TokenFile)
		if err != nil {
			return Identity{}, errors.Join(ErrUnavailable, err)
		}
		if !auth.BearerMatches(header, want) {
			return Identity{}, ErrSignedOut
		}
		return Identity{Username: TokenUser, Role: config.RoleAdmin, Token: true}, nil
	}
	se, err := d.session(r)
	switch {
	case err == nil:
		if !headerSent(r) {
			return Identity{}, ErrNoRequestHeader
		}
		return Identity{Username: se.Username, Groups: se.Groups, Role: RoleOf(d.Access, se.Username, se.Groups)}, nil
	case !errors.Is(err, ErrSignedOut):
		return Identity{}, err
	case throughSetup(r):
		// The browser sends nothing that proves who, so a page on another
		// site could post to localhost; it can't add the header.
		if !headerSent(r) {
			return Identity{}, ErrNoRequestHeader
		}
		return Identity{Username: SetupUser, Role: config.RoleAdmin, Setup: true}, nil
	}
	return Identity{}, ErrSignedOut
}

// session is the live session r's cookie names.
func (d *Directory) session(r *http.Request) (store.Session, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || d.Sessions == nil {
		return store.Session{}, ErrSignedOut
	}
	se, err := d.Sessions.Session(r.Context(), c.Value, d.now())
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, ErrSignedOut
	}
	if err != nil {
		return store.Session{}, errors.Join(ErrUnavailable, err)
	}
	return se, nil
}

// headerSent reports whether r reads, or carries RequestHeader.
func headerSent(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get(RequestHeader) != ""
}

func (d *Directory) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

type identityKey struct{}

// WithIdentity returns ctx carrying i.
func WithIdentity(ctx context.Context, i Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, i)
}

// From returns the identity ctx carries, if any.
func From(ctx context.Context) (Identity, bool) {
	i, ok := ctx.Value(identityKey{}).(Identity)
	return i, ok
}
