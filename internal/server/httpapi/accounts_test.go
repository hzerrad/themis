package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// fresh is a new install as the core serves it, which tests reach as
// localhost, as kubectl port-forward does: the API, the initial admin
// password, password sign-in and sign-out.
type fresh struct {
	t       *testing.T
	handler http.Handler
}

const initialPassword = "a long initial password"

func newFresh(t *testing.T) *fresh {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(t.TempDir(), "initial-admin")
	if err := os.WriteFile(password, []byte(initialPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	clock := func() time.Time { return now }
	setup := &identity.Setup{Store: s, Logger: log}
	mux := http.NewServeMux()
	(&httpapi.API{
		Store: s, TokenFile: file, Sessions: s, Accounts: s, Logger: log, Now: clock, Namespace: "themis",
		Setup: setup, InitialAdmin: &config.InitialAdmin{PasswordFile: password, Secret: "themis-initial-admin"},
	}).Register(mux)
	(&identity.InitialAdmin{PasswordFile: password, Setup: setup, Store: s, Logger: log, Now: clock}).Register(mux)
	(&identity.Passwords{Store: s, Logger: log, Now: clock}).Register(mux)
	(&identity.SignOut{Store: s, Logger: log}).Register(mux)
	return &fresh{t: t, handler: mux}
}

// send sends a request from localhost, with session's cookie if any, and the
// header the UI adds.
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
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f *fresh) signIn(username, password string) *http.Cookie {
	f.t.Helper()
	return f.session(username, f.send(http.MethodPost, "/auth/password", nil, map[string]string{"username": username, "password": password}))
}

func (f *fresh) signInFirst() *http.Cookie {
	f.t.Helper()
	return f.session("the initial admin", f.send(http.MethodPost, "/auth/initial-admin", nil, map[string]string{"password": initialPassword}))
}

// session is the session rec started, as who.
func (f *fresh) session(who string, rec *httptest.ResponseRecorder) *http.Cookie {
	f.t.Helper()
	if rec.Code != http.StatusNoContent {
		f.t.Fatalf("%s signing in = %d %q", who, rec.Code, rec.Body)
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

// Reaching a new install from localhost, as kubectl port-forward does, grants
// nothing. The first admin signs in with the initial admin password, makes
// their admin account and signs in with it, which retires the password.
func TestAccounts_SetupFlow(t *testing.T) {
	f := newFresh(t)
	if code, me := f.me(nil); code != http.StatusUnauthorized {
		t.Fatalf("me from localhost, signed out = %d %v, want 401", code, me)
	}
	want := `{"oidc":false,"password":false,"token":true,"initialAdmin":{"command":"kubectl -n themis get secret themis-initial-admin -o jsonpath='{.data.password}' | base64 -d"}}`
	if rec := f.send(http.MethodGet, "/v1/sign-in", nil, nil); rec.Body.String() != want {
		t.Errorf("sign-in methods on a new install = %s, want %s", rec.Body, want)
	}
	first := f.signInFirst()
	if code, me := f.me(first); code != http.StatusOK || me["username"] != "initial-admin" || me["role"] != config.RoleAdmin || me["initialAdmin"] != true {
		t.Fatalf("me as the initial admin = %d %v", code, me)
	}
	expect(t, "creating the first admin", f.send(http.MethodPost, "/v1/accounts", first, map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	root := f.signIn("root", rootPassword)
	if code, me := f.me(root); code != http.StatusOK || me["username"] != "root" || me["role"] != config.RoleAdmin || me["account"] != true {
		t.Fatalf("me as root = %d %v", code, me)
	}
	if code, _ := f.me(first); code != http.StatusUnauthorized {
		t.Errorf("the initial admin's session once root signed in = %d, want 401", code)
	}
	expect(t, "the initial admin password once root signed in", f.send(http.MethodPost, "/auth/initial-admin", nil, map[string]string{"password": initialPassword}), http.StatusForbidden)
	if rec := f.send(http.MethodGet, "/v1/sign-in", nil, nil); rec.Body.String() != `{"oidc":false,"password":true,"token":true}` {
		t.Errorf("sign-in methods once set up = %s", rec.Body)
	}
}

func TestAccounts_AdminsManageThem(t *testing.T) {
	f := newFresh(t)
	expect(t, "creating root", f.send(http.MethodPost, "/v1/accounts", f.signInFirst(), map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
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
	expect(t, "deleting an unknown account", f.send(http.MethodDelete, "/v1/accounts/nobody", root, nil), http.StatusNotFound)

	b := f.signIn("bob", "bob's long password")
	expect(t, "bob listing accounts", f.send(http.MethodGet, "/v1/accounts", b, nil), http.StatusForbidden)
	expect(t, "root making bob a viewer", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"role": "viewer"}), http.StatusNoContent)
	if _, me := f.me(b); me["role"] != config.RoleViewer {
		t.Errorf("bob's role after the change = %v, want viewer", me["role"])
	}

	// An admin resetting a password ends that person's sessions, as their
	// own change does.
	expect(t, "root changing nothing about bob", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{}), http.StatusBadRequest)
	expect(t, "root giving bob a short password", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"password": "short"}), http.StatusBadRequest)
	expect(t, "root resetting bob's password", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"password": "bob's reset long password"}), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's session after an admin reset his password = %d, want 401", code)
	}
	b = f.signIn("bob", "bob's reset long password")

	expect(t, "bob, with a wrong current password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "a guess", "new": "bob's new long password"}), http.StatusForbidden)
	expect(t, "bob choosing a short password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's reset long password", "new": "short"}), http.StatusBadRequest)
	expect(t, "bob changing his password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's reset long password", "new": "bob's new long password"}), http.StatusNoContent)
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

// Changing your own password is under the same lock as signing in, so a
// stolen session can't be used to guess the password.
func TestAccounts_ChangingYourPasswordLocksAfterWrongTries(t *testing.T) {
	f := newFresh(t)
	expect(t, "creating root", f.send(http.MethodPost, "/v1/accounts", f.signInFirst(), map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	root := f.signIn("root", rootPassword)
	change := func(current string) *httptest.ResponseRecorder {
		return f.send(http.MethodPost, "/v1/me/password", root, map[string]string{"current": current, "new": "root's new long password"})
	}
	for i := range 10 {
		expect(t, fmt.Sprintf("wrong current password %d", i+1), change("a guess"), http.StatusForbidden)
	}
	rec := change(rootPassword)
	expect(t, "the right password once locked", rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "locked") {
		t.Errorf("a locked change says %q, want it to say the account is locked", rec.Body)
	}
	expect(t, "signing in once locked", f.send(http.MethodPost, "/auth/password", nil, map[string]string{"username": "root", "password": rootPassword}), http.StatusUnauthorized)
}
