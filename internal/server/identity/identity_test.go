package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

var bindings = []config.AccessBinding{
	{Role: config.RoleViewer, Groups: []string{"everyone"}},
	{Role: config.RoleApprover, Groups: []string{"sre"}},
	{Role: config.RoleAdmin, Users: []string{"root@example.com"}},
}

func TestRoleOf_IsTheMostTrustedMatch(t *testing.T) {
	for _, tc := range []struct {
		user   string
		groups []string
		want   string
	}{
		{"bob@example.com", []string{"everyone"}, config.RoleViewer},
		{"bob@example.com", []string{"everyone", "sre"}, config.RoleApprover},
		{"root@example.com", []string{"everyone"}, config.RoleAdmin},
		{"eve@example.com", []string{"contractors"}, ""},
	} {
		if got := identity.RoleOf(bindings, tc.user, tc.groups); got != tc.want {
			t.Errorf("RoleOf(%s, %v) = %q, want %q", tc.user, tc.groups, got, tc.want)
		}
	}
}

func TestIdentity_Can(t *testing.T) {
	approver := identity.Identity{Role: config.RoleApprover}
	if !approver.Can(config.RoleViewer) || !approver.Can(config.RoleResponder) || !approver.Can(config.RoleApprover) {
		t.Error("an approver can't do what less trusted roles can")
	}
	if approver.Can(config.RoleAdmin) {
		t.Error("an approver can act as an admin")
	}
	if (identity.Identity{}).Can(config.RoleViewer) {
		t.Error("someone with no role can view")
	}
}

type sessions map[string]store.Session

func (s sessions) Session(_ context.Context, id string, at time.Time) (store.Session, error) {
	se, ok := s[id]
	if !ok || !se.ExpiresAt.After(at) {
		return store.Session{}, store.ErrNotFound
	}
	return se, nil
}

func directory(t *testing.T) *identity.Directory {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("admin-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &identity.Directory{
		TokenFile: token, Access: bindings, Now: func() time.Time { return now },
		Sessions: sessions{"live": {Username: "bob@example.com", Groups: []string{"sre"}, ExpiresAt: now.Add(time.Hour)}},
	}
}

func TestIdentify(t *testing.T) {
	d := directory(t)
	for name, tc := range map[string]struct {
		method, url, auth, cookie, header string
		want                              identity.Identity
		err                               error
	}{
		"the admin token":               {method: http.MethodGet, auth: "Bearer admin-secret", want: identity.Identity{Username: "admin", Role: config.RoleAdmin, Token: true}},
		"a wrong token":                 {method: http.MethodGet, auth: "Bearer guess", err: identity.ErrSignedOut},
		"a wrong token, with a session": {method: http.MethodGet, auth: "Bearer guess", cookie: "live", err: identity.ErrSignedOut},
		"a session":                     {method: http.MethodGet, cookie: "live", want: identity.Identity{Username: "bob@example.com", Groups: []string{"sre"}, Role: config.RoleApprover}},
		"an unknown session":            {method: http.MethodGet, cookie: "stolen", err: identity.ErrSignedOut},
		"nothing":                       {method: http.MethodGet, err: identity.ErrSignedOut},
		// Without the setup door, nothing is an admin for free.
		"nothing, from localhost": {method: http.MethodGet, url: "http://localhost:8090/v1/me", err: identity.ErrSignedOut},
		"a change with a session": {method: http.MethodPost, cookie: "live", header: "1", want: identity.Identity{Username: "bob@example.com", Groups: []string{"sre"}, Role: config.RoleApprover}},
		// A form on another site can post with the cookie, but can't add the header.
		"a change without the header": {method: http.MethodPost, cookie: "live", err: identity.ErrNoRequestHeader},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.url == "" {
				tc.url = "/v1/incidents"
			}
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.url, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: tc.cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			}
			if tc.header != "" {
				req.Header.Set(identity.RequestHeader, tc.header)
			}
			got, err := d.Identify(req)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if got.Username != tc.want.Username || got.Role != tc.want.Role || got.Token != tc.want.Token {
				t.Errorf("identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Without OIDC, a cookie proves nothing.
func TestIdentify_NoSessionsWithoutOIDC(t *testing.T) {
	d := directory(t)
	d.Sessions = nil
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/incidents", nil)
	req.AddCookie(&http.Cookie{Name: identity.SessionCookie, Value: "live", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if _, err := d.Identify(req); !errors.Is(err, identity.ErrSignedOut) {
		t.Errorf("err = %v, want ErrSignedOut", err)
	}
}
