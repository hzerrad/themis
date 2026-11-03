package people_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/people"
	"github.com/hzerrad/themis/internal/store"
)

// accountStore keeps accounts and sessions in memory.
type accountStore struct {
	*memStore
	mu       sync.Mutex
	accounts map[string]store.Account
}

func (a *accountStore) Account(_ context.Context, username string) (store.Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	acc, ok := a.accounts[username]
	if !ok {
		return store.Account{}, store.ErrNotFound
	}
	return acc, nil
}

func (a *accountStore) PasswordFailed(_ context.Context, username string, now time.Time, maxFailures int, lockFor time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	acc, ok := a.accounts[username]
	if !ok {
		return nil
	}
	acc.FailedAttempts++
	if acc.FailedAttempts >= maxFailures {
		acc.FailedAttempts, acc.LockedUntil = 0, now.Add(lockFor)
	}
	a.accounts[username] = acc
	return nil
}

func (a *accountStore) PasswordSucceeded(_ context.Context, username string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	acc := a.accounts[username]
	acc.FailedAttempts, acc.LockedUntil = 0, time.Time{}
	a.accounts[username] = acc
	return nil
}

const alicePassword = "correct horse battery"

func passwordRig(t *testing.T) (*accountStore, http.Handler) {
	t.Helper()
	hash, err := people.HashPassword(t.Context(), alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	s := &accountStore{memStore: newMemStore(), accounts: map[string]store.Account{"alice": {Username: "alice", PasswordHash: hash, Role: config.RoleApprover}}}
	mux := http.NewServeMux()
	(&people.Passwords{Store: s, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}).Register(mux)
	return s, mux
}

func signInWith(t *testing.T, h http.Handler, host, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/auth/password", strings.NewReader(body))
	req.Header.Set(people.RequestHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPasswords_SignIn(t *testing.T) {
	s, h := passwordRig(t)
	rec := signInWith(t, h, "themis.example.com", "alice", alicePassword)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign-in = %d %q", rec.Code, rec.Body)
	}
	c := cookieNamed(t, rec, people.SessionCookie)
	if !c.Secure || !c.HttpOnly {
		t.Errorf("cookie away from localhost = %+v, want secure and HTTP-only", c)
	}
	se, err := s.Session(t.Context(), c.Value, now)
	if err != nil || se.Subject != store.AccountSubjectPrefix+"alice" || se.Username != "alice" {
		t.Errorf("session = %+v, %v", se, err)
	}
	// On localhost, over plain HTTP, a secure cookie would never come back.
	if c := cookieNamed(t, signInWith(t, h, "localhost:8090", "alice", alicePassword), people.SessionCookie); c.Secure {
		t.Error("cookie on localhost is secure, so plain HTTP can't sign in")
	}
}

// A wrong password and an unknown username say the same thing.
func TestPasswords_Refuses(t *testing.T) {
	_, h := passwordRig(t)
	wrong := signInWith(t, h, "themis.example.com", "alice", "not the password")
	unknown := signInWith(t, h, "themis.example.com", "mallory", alicePassword)
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized || wrong.Body.String() != unknown.Body.String() {
		t.Errorf("wrong password = %d %q; unknown username = %d %q; want the same 401", wrong.Code, wrong.Body, unknown.Code, unknown.Body)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/password", strings.NewReader(`{"username":"alice","password":"`+alicePassword+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("sign-in without the request header = %d, want 403", rec.Code)
	}
}

func TestPasswords_LockAfterWrongTries(t *testing.T) {
	s, h := passwordRig(t)
	for range 10 {
		signInWith(t, h, "themis.example.com", "alice", "a wrong guess")
	}
	if rec := signInWith(t, h, "themis.example.com", "alice", alicePassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the right password while locked = %d, want 401", rec.Code)
	}
	s.mu.Lock()
	acc := s.accounts["alice"]
	acc.LockedUntil = now.Add(-time.Second)
	s.accounts["alice"] = acc
	s.mu.Unlock()
	if rec := signInWith(t, h, "themis.example.com", "alice", alicePassword); rec.Code != http.StatusNoContent {
		t.Errorf("the right password after the lock = %d, want 204", rec.Code)
	}
}

// An account's role comes from the account on every request, so a change or
// deletion applies at once.
func TestIdentify_Account(t *testing.T) {
	s, h := passwordRig(t)
	c := cookieNamed(t, signInWith(t, h, "themis.example.com", "alice", alicePassword), people.SessionCookie)
	d := &people.Directory{Sessions: s, Accounts: s, Now: func() time.Time { return now }}
	identify := func() (people.Identity, error) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
		req.AddCookie(c)
		return d.Identify(req)
	}
	if who, err := identify(); err != nil || who.Username != "alice" || who.Role != config.RoleApprover || !who.Account {
		t.Fatalf("identity = %+v, %v", who, err)
	}
	s.mu.Lock()
	acc := s.accounts["alice"]
	acc.Role = config.RoleViewer
	s.accounts["alice"] = acc
	s.mu.Unlock()
	if who, _ := identify(); who.Role != config.RoleViewer {
		t.Errorf("role after a change = %q, want viewer", who.Role)
	}
	s.mu.Lock()
	delete(s.accounts, "alice")
	s.mu.Unlock()
	if _, err := identify(); !errors.Is(err, people.ErrSignedOut) {
		t.Errorf("after the account is deleted: %v, want signed out", err)
	}
}

func TestPassword_HashAndRules(t *testing.T) {
	hash, err := people.HashPassword(t.Context(), alicePassword)
	if err != nil || !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("hash = %q, %v", hash, err)
	}
	if ok, err := people.CheckPassword(t.Context(), hash, alicePassword); err != nil || !ok {
		t.Errorf("the right password: %v, %v", ok, err)
	}
	if ok, _ := people.CheckPassword(t.Context(), hash, alicePassword+"!"); ok {
		t.Error("a wrong password matched")
	}
	for name, err := range map[string]error{
		"too short":       people.CheckNewPassword("alice", "short"),
		"the username":    people.CheckNewPassword("alice-in-wonderland", "Alice-in-Wonderland"),
		"uppercase name":  people.CheckUsername("Alice"),
		"reserved name":   people.CheckUsername("setup"),
		"an email":        people.CheckUsername("alice@example.com"),
		"one letter name": people.CheckUsername("a"),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := people.CheckUsername("alice.b-2"); err != nil {
		t.Errorf("a good username: %v", err)
	}
	if err := people.CheckNewPassword("alice", alicePassword); err != nil {
		t.Errorf("a good password: %v", err)
	}
}
