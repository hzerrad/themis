package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/identity"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// fresh is a new install as the core serves it: the API, password sign-in
// and sign-out, behind the setup door, which tests reach as localhost.
type fresh struct {
	t    *testing.T
	door http.Handler
}

func newFresh(t *testing.T) *fresh {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	(&httpapi.API{Store: s, TokenFile: file, Sessions: s, Accounts: s, Setup: s, Logger: log, Now: func() time.Time { return now }}).Register(mux)
	(&identity.Passwords{Store: s, Logger: log, Now: func() time.Time { return now }}).Register(mux)
	(&identity.SignOut{Store: s, Logger: log}).Register(mux)
	return &fresh{t: t, door: &identity.Door{Store: s, Next: mux, Logger: log}}
}

// send sends a request through the door, with session's cookie if any, and
// the header the UI adds.
func (f *fresh) send(method, path string, session *http.Cookie, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(f.t.Context(), method, "http://localhost:8090"+path, &b)
	req.Header.Set("X-Themis-Request", "1")
	if session != nil {
		req.AddCookie(session)
	}
	rec := httptest.NewRecorder()
	f.door.ServeHTTP(rec, req)
	return rec
}

func (f *fresh) signIn(username, password string) *http.Cookie {
	f.t.Helper()
	rec := f.send(http.MethodPost, "/auth/password", nil, map[string]string{"username": username, "password": password})
	if rec.Code != http.StatusNoContent {
		f.t.Fatalf("%s signing in = %d %q", username, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == identity.SessionCookie {
			return c
		}
	}
	f.t.Fatal("no session cookie")
	return nil
}

func (f *fresh) me(session *http.Cookie) (int, map[string]any) {
	f.t.Helper()
	rec := f.send(http.MethodGet, "/v1/me", session, nil)
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	return rec.Code, me
}

func expect(t *testing.T, what string, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Errorf("%s = %d %q, want %d", what, rec.Code, rec.Body, code)
	}
}

const rootPassword = "a long root password"

// A new user comes in through port-forward, makes the first admin account,
// signs in with it, and from then on port-forward alone lets no one in.
func TestAccounts_SetupFlow(t *testing.T) {
	f := newFresh(t)
	if code, me := f.me(nil); code != http.StatusOK || me["setup"] != true {
		t.Fatalf("me through the door = %d %v, want the setup admin", code, me)
	}
	if rec := f.send(http.MethodGet, "/v1/sign-in", nil, nil); !strings.Contains(rec.Body.String(), `"password":false`) {
		t.Errorf("sign-in methods before any account = %q", rec.Body)
	}
	expect(t, "creating the first admin", f.send(http.MethodPost, "/v1/accounts", nil, map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	if rec := f.send(http.MethodGet, "/v1/sign-in", nil, nil); !strings.Contains(rec.Body.String(), `"password":true`) {
		t.Errorf("sign-in methods after an account = %q", rec.Body)
	}
	root := f.signIn("root", rootPassword)
	if code, me := f.me(root); code != http.StatusOK || me["username"] != "root" || me["role"] != config.RoleAdmin || me["account"] != true {
		t.Fatalf("me as root = %d %v", code, me)
	}
	if code, _ := f.me(nil); code != http.StatusUnauthorized {
		t.Errorf("me through the door after setup = %d, want 401", code)
	}
}

func TestAccounts_AdminsManageThem(t *testing.T) {
	f := newFresh(t)
	expect(t, "creating root", f.send(http.MethodPost, "/v1/accounts", nil, map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	root := f.signIn("root", rootPassword)

	bob := map[string]string{"username": "bob", "role": "approver", "password": "bob's long password"}
	expect(t, "creating bob", f.send(http.MethodPost, "/v1/accounts", root, bob), http.StatusCreated)
	expect(t, "creating bob again", f.send(http.MethodPost, "/v1/accounts", root, bob), http.StatusConflict)
	rec := f.send(http.MethodPost, "/v1/accounts", root, map[string]string{"username": "Carol", "role": "boss", "password": "short"})
	expect(t, "a bad account", rec, http.StatusBadRequest)
	if body := rec.Body.String(); !strings.Contains(body, "A username is") || !strings.Contains(body, "The role must be") || !strings.Contains(body, "A password needs") {
		t.Errorf("a bad account says %q, want every problem as a sentence", body)
	}

	expect(t, "root changing their own role", f.send(http.MethodPatch, "/v1/accounts/root", root, map[string]string{"role": "viewer"}), http.StatusForbidden)
	expect(t, "root deleting themselves", f.send(http.MethodDelete, "/v1/accounts/root", root, nil), http.StatusForbidden)
	expect(t, "changing an unknown account", f.send(http.MethodPatch, "/v1/accounts/nobody", root, map[string]string{"role": "viewer"}), http.StatusNotFound)

	b := f.signIn("bob", "bob's long password")
	expect(t, "bob listing accounts", f.send(http.MethodGet, "/v1/accounts", b, nil), http.StatusForbidden)
	expect(t, "root making bob a viewer", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"role": "viewer"}), http.StatusNoContent)
	if _, me := f.me(b); me["role"] != config.RoleViewer {
		t.Errorf("bob's role after the change = %v, want viewer", me["role"])
	}

	expect(t, "bob, with a wrong current password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "a guess", "new": "bob's new long password"}), http.StatusForbidden)
	expect(t, "bob changing his password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's long password", "new": "bob's new long password"}), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's old session after a new password = %d, want 401", code)
	}
	b = f.signIn("bob", "bob's new long password")

	rec = f.send(http.MethodGet, "/v1/accounts", root, nil)
	if !strings.Contains(rec.Body.String(), `"username":"bob"`) || strings.Contains(rec.Body.String(), "argon2") {
		t.Errorf("accounts = %q, want bob and no password hashes", rec.Body)
	}
	expect(t, "deleting bob", f.send(http.MethodDelete, "/v1/accounts/bob", root, nil), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's session after his account was deleted = %d, want 401", code)
	}
}
