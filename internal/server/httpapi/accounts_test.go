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

	"github.com/jackc/pgx/v5/pgxpool"

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
	pool    *pgxpool.Pool
}

const initialPassword = "a long initial password"

// newFresh is a new install. With access, people also sign in through OIDC,
// with the roles access gives them.
func newFresh(t *testing.T, access ...config.AccessBinding) *fresh {
	t.Helper()
	s, pool := pgtest.NewStore(t)
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
	setup, gate := &identity.Setup{Store: s, Logger: log}, &identity.Gate{Store: s}
	mux := http.NewServeMux()
	(&httpapi.API{
		Store: s, TokenFile: file, Sessions: s, Accounts: s, Logger: log, Now: clock, Namespace: "themis", Gate: gate,
		Setup: setup, InitialAdmin: &config.InitialAdmin{PasswordFile: password, Secret: "themis-initial-admin"},
		OIDC: len(access) > 0, Access: access,
	}).Register(mux)
	(&identity.InitialAdmin{PasswordFile: password, Setup: setup, Gate: gate, Store: s, Logger: log, Now: clock}).Register(mux)
	(&identity.Passwords{Store: s, Logger: log, Now: clock}).Register(mux)
	(&identity.SignOut{Store: s, Logger: log}).Register(mux)
	return &fresh{t: t, handler: mux, pool: pool}
}

// send sends a request from localhost, with session's cookie if any, and the
// header the UI adds.
func (f *fresh) send(method, path string, session *http.Cookie, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.serve(method, path, body, func(req *http.Request) {
		if session != nil {
			req.AddCookie(session)
		}
	})
}

// withToken sends a request with the break-glass token.
func (f *fresh) withToken(method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.serve(method, path, body, func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+token) })
}

func (f *fresh) serve(method, path string, body any, prepare func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(f.t.Context(), method, "http://localhost:8090"+path, &b)
	req.Header.Set("X-Themis-Request", "1")
	prepare(req)
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

// choose signs username in with the temporary password an admin set, chooses
// own, and signs in with it.
func (f *fresh) choose(username, temporary, own string) *http.Cookie {
	f.t.Helper()
	rec := f.send(http.MethodPost, "/v1/me/password", f.signIn(username, temporary), map[string]string{"current": temporary, "new": own})
	if rec.Code != http.StatusNoContent {
		f.t.Fatalf("%s choosing a password = %d %q", username, rec.Code, rec.Body)
	}
	return f.signIn(username, own)
}

// admin is root, an admin account the initial admin made, signed in with a
// password of root's own.
func (f *fresh) admin() *http.Cookie {
	f.t.Helper()
	expect(f.t, "creating root", f.send(http.MethodPost, "/v1/accounts", f.signInFirst(), map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	return f.choose("root", rootPassword, rootOwnPassword)
}

func (f *fresh) me(session *http.Cookie) (int, map[string]any) {
	f.t.Helper()
	rec := f.send(http.MethodGet, "/v1/me", session, nil)
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	return rec.Code, me
}

// record is the record of account changes, newest first, one line each.
func (f *fresh) record(session *http.Cookie) []string {
	f.t.Helper()
	rec := f.send(http.MethodGet, "/v1/audit/accounts", session, nil)
	var got struct {
		Events []struct {
			At                            time.Time
			Actor, Action, Username, Role string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		f.t.Fatalf("record = %d %q: %v", rec.Code, rec.Body, err)
	}
	var lines []string
	for _, e := range got.Events {
		if !e.At.Equal(now) {
			f.t.Errorf("%s at %s, want %s", e.Action, e.At, now)
		}
		lines = append(lines, strings.TrimSpace(strings.Join([]string{e.Actor, e.Action, e.Username, e.Role}, " ")))
	}
	return lines
}

func expect(t *testing.T, what string, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Errorf("%s = %d %q, want %d", what, rec.Code, rec.Body, code)
	}
}

func expectRecord(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("record =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const (
	rootPassword    = "a long root password"
	rootOwnPassword = "a long password of root"
)

// Reaching a new install from localhost, as kubectl port-forward does, grants
// nothing. The first admin signs in with the initial admin password, whose
// only use is making the first admin account. Its password is temporary, so
// its holder chooses their own, and signing in with it retires the initial
// admin password.
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
	if code, me := f.me(first); code != http.StatusOK || me["username"] != "initial-admin" || me["role"] != config.RoleAdmin || me["initialAdmin"] != true || me["closed"] != true {
		t.Fatalf("me as the initial admin = %d %v", code, me)
	}
	expect(t, "the initial admin making a viewer first", f.send(http.MethodPost, "/v1/accounts", first, map[string]string{"username": "bob", "role": "viewer", "password": rootPassword}), http.StatusConflict)
	expect(t, "creating the first admin", f.send(http.MethodPost, "/v1/accounts", first, map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	for _, rec := range []*httptest.ResponseRecorder{
		f.send(http.MethodPost, "/v1/accounts", first, map[string]string{"username": "carol", "role": "admin", "password": rootPassword}),
		f.send(http.MethodGet, "/v1/accounts", first, nil),
	} {
		if rec.Code != http.StatusForbidden || strings.TrimSpace(rec.Body.String()) != "Refused. The initial admin password only creates the first admin account." {
			t.Errorf("the initial admin once root exists = %d %q, want 403", rec.Code, rec.Body)
		}
	}
	if rec := f.send(http.MethodGet, "/v1/sign-in", nil, nil); rec.Body.String() != `{"oidc":false,"password":true,"token":true}` {
		t.Errorf("sign-in methods once root exists = %s, want no initial admin password", rec.Body)
	}
	temporary := f.signIn("root", rootPassword)
	if code, me := f.me(temporary); code != http.StatusOK || me["temporaryPassword"] != true || me["closed"] != false {
		t.Fatalf("me as root, with the password the initial admin chose = %d %v, want it temporary", code, me)
	}
	if code, _ := f.me(first); code != http.StatusOK {
		t.Errorf("the initial admin's session before root chose a password = %d, want 200: setup isn't complete", code)
	}
	root := f.choose("root", rootPassword, rootOwnPassword)
	if code, me := f.me(root); code != http.StatusOK || me["username"] != "root" || me["role"] != config.RoleAdmin || me["account"] != true || me["temporaryPassword"] != false {
		t.Fatalf("me as root = %d %v", code, me)
	}
	if code, _ := f.me(first); code != http.StatusUnauthorized {
		t.Errorf("the initial admin's session once root signed in = %d, want 401", code)
	}
	expect(t, "the initial admin password once root signed in", f.send(http.MethodPost, "/auth/initial-admin", nil, map[string]string{"password": initialPassword}), http.StatusForbidden)
	expectRecord(t, f.record(root), []string{
		"root initial_admin_retired",
		"root password_changed root",
		"initial-admin account_created root admin",
		"initial-admin initial_admin_signed_in",
	})
}

func TestAccounts_AdminsManageThem(t *testing.T) {
	f := newFresh(t)
	root := f.admin()

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

	// The password root chose for bob is temporary: until bob chooses his
	// own, he may do nothing else.
	b := f.signIn("bob", "bob's long password")
	rec = f.send(http.MethodGet, "/v1/incidents", b, nil)
	expect(t, "bob listing incidents with a temporary password", rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Refused. The password is temporary.") {
		t.Errorf("a temporary password says %q", rec.Body)
	}
	expect(t, "bob keeping the temporary password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's long password", "new": "bob's long password"}), http.StatusBadRequest)
	b = f.choose("bob", "bob's long password", "bob's own long password")
	expect(t, "bob listing accounts", f.send(http.MethodGet, "/v1/accounts", b, nil), http.StatusForbidden)
	expect(t, "root making bob a viewer", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"role": "viewer"}), http.StatusNoContent)
	if _, me := f.me(b); me["role"] != config.RoleViewer {
		t.Errorf("bob's role after the change = %v, want viewer", me["role"])
	}

	// An admin resetting a password sets a temporary one, and ends that
	// person's sessions, as their own change does.
	expect(t, "root changing nothing about bob", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{}), http.StatusBadRequest)
	expect(t, "root giving bob a short password", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"password": "short"}), http.StatusBadRequest)
	expect(t, "root resetting bob's password", f.send(http.MethodPatch, "/v1/accounts/bob", root, map[string]string{"password": "bob's reset long password"}), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's session after an admin reset his password = %d, want 401", code)
	}
	if rec := f.send(http.MethodGet, "/v1/accounts", root, nil); !strings.Contains(rec.Body.String(), `"username":"bob","role":"viewer","locked":false,"temporaryPassword":true`) || strings.Contains(rec.Body.String(), "argon2") {
		t.Errorf("accounts = %q, want bob's password temporary, and no password hashes", rec.Body)
	}
	b = f.signIn("bob", "bob's reset long password")
	expect(t, "bob, with a wrong current password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "a guess", "new": "bob's new long password"}), http.StatusForbidden)
	expect(t, "bob choosing a short password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's reset long password", "new": "short"}), http.StatusBadRequest)
	expect(t, "bob changing his password", f.send(http.MethodPost, "/v1/me/password", b, map[string]string{"current": "bob's reset long password", "new": "bob's new long password"}), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's old session after a new password = %d, want 401", code)
	}
	b = f.signIn("bob", "bob's new long password")

	// root is the last admin, so the break-glass token can't take that away.
	for _, rec := range []*httptest.ResponseRecorder{
		f.withToken(http.MethodPatch, "/v1/accounts/root", map[string]string{"role": "approver"}),
		f.withToken(http.MethodDelete, "/v1/accounts/root", nil),
	} {
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Refused. root is the last admin account." {
			t.Errorf("the token removing the last admin = %d %q, want 409", rec.Code, rec.Body)
		}
	}
	expect(t, "deleting bob", f.send(http.MethodDelete, "/v1/accounts/bob", root, nil), http.StatusNoContent)
	if code, _ := f.me(b); code != http.StatusUnauthorized {
		t.Errorf("bob's session after his account was deleted = %d, want 401", code)
	}
	expectRecord(t, f.record(root)[:6], []string{
		"root account_deleted bob",
		"bob password_changed bob",
		"root temporary_password_set bob",
		"root role_changed bob viewer",
		"bob password_changed bob",
		"root account_created bob approver",
	})
}

// The last admin account stays even when the identity provider signs an
// admin in: Themis always needs an admin account of its own.
func TestAccounts_TheLastAdminAccountWithAnAdminThroughOIDC(t *testing.T) {
	f := newFresh(t, config.AccessBinding{Role: config.RoleAdmin, Users: []string{"alice@example.com"}})
	f.admin()
	rec := f.withToken(http.MethodDelete, "/v1/accounts/root", nil)
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != "Refused. root is the last admin account." {
		t.Errorf("the token deleting root = %d %q, want 409", rec.Code, rec.Body)
	}
}

// While no admin account exists, Themis serves nothing but the making of the
// first, to anyone, the break-glass token included. It closes again if the
// admin accounts are ever gone, as from a database restored without them.
func TestAccounts_ClosedWithoutAnAdminAccount(t *testing.T) {
	f := newFresh(t)
	const id = "0192f0c1-0000-7000-8000-000000000001"
	closed := func(when string) {
		t.Helper()
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/v1/incidents"},
			{http.MethodGet, "/v1/incidents/" + id},
			{http.MethodPost, "/v1/incidents/" + id + "/proposals"},
			{http.MethodPost, "/v1/actions/" + id + "/approve"},
			{http.MethodPost, "/v1/actions/" + id + "/deny"},
			{http.MethodGet, "/v1/accounts"},
			{http.MethodPatch, "/v1/accounts/root"},
			{http.MethodDelete, "/v1/accounts/root"},
			{http.MethodGet, "/v1/audit/accounts"},
			{http.MethodPost, "/v1/me/password"},
		} {
			rec := f.withToken(r.method, r.path, map[string]string{})
			if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != identity.ClosedMessage {
				t.Errorf("%s: %s %s = %d %q, want 409", when, r.method, r.path, rec.Code, rec.Body)
			}
		}
		var me map[string]any
		if rec := f.withToken(http.MethodGet, "/v1/me", nil); json.Unmarshal(rec.Body.Bytes(), &me) != nil || me["closed"] != true {
			t.Errorf("%s: me = %d %q, want closed", when, rec.Code, rec.Body)
		}
	}
	closed("on a new install")
	expect(t, "the token making a viewer first", f.withToken(http.MethodPost, "/v1/accounts", map[string]string{"username": "bob", "role": "viewer", "password": rootPassword}), http.StatusConflict)
	expect(t, "the token making the first admin", f.withToken(http.MethodPost, "/v1/accounts", map[string]string{"username": "root", "role": "admin", "password": rootPassword}), http.StatusCreated)
	expect(t, "incidents once an admin account exists", f.withToken(http.MethodGet, "/v1/incidents", nil), http.StatusOK)
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM accounts`); err != nil {
		t.Fatal(err)
	}
	closed("once the admin accounts are gone")
}

// Changing your own password is under the same lock as signing in, so a
// stolen session can't be used to guess the password.
func TestAccounts_ChangingYourPasswordLocksAfterWrongTries(t *testing.T) {
	f := newFresh(t)
	root := f.admin()
	change := func(current string) *httptest.ResponseRecorder {
		return f.send(http.MethodPost, "/v1/me/password", root, map[string]string{"current": current, "new": "root's new long password"})
	}
	for i := range 10 {
		expect(t, fmt.Sprintf("wrong current password %d", i+1), change("a guess"), http.StatusForbidden)
	}
	rec := change(rootOwnPassword)
	expect(t, "the right password once locked", rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "locked") {
		t.Errorf("a locked change says %q, want it to say the account is locked", rec.Body)
	}
	expect(t, "signing in once locked", f.send(http.MethodPost, "/auth/password", nil, map[string]string{"username": "root", "password": rootOwnPassword}), http.StatusUnauthorized)
}
