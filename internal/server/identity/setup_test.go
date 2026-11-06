package identity_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

// firstRun is a new install in memory: whether setup finished, the initial
// admin password's wrong tries, and sessions.
type firstRun struct {
	*memStore
	finished    bool
	err         error
	asked       int
	failures    int
	lockedUntil time.Time
}

func (f *firstRun) SetupFinished(context.Context) (bool, error) {
	f.asked++
	return f.finished, f.err
}

func (f *firstRun) FinishSetup(context.Context, string, time.Time) (bool, error) {
	done := !f.finished
	f.finished = true
	return done, nil
}

func (f *firstRun) InitialAdminLockedUntil(context.Context) (time.Time, error) {
	return f.lockedUntil, nil
}

func (f *firstRun) InitialAdminPasswordFailed(_ context.Context, now time.Time, maxFailures int, lockFor time.Duration) error {
	f.failures++
	if f.failures >= maxFailures {
		f.failures, f.lockedUntil = 0, now.Add(lockFor)
	}
	return nil
}

func (f *firstRun) InitialAdminPasswordSucceeded(context.Context) error {
	f.failures, f.lockedUntil = 0, time.Time{}
	return nil
}

func (f *firstRun) RecordAccountEvent(context.Context, store.AccountEvent) error { return nil }

const initialPassword = "a long initial password"

type firstRunRig struct {
	store   *firstRun
	setup   *identity.Setup
	handler http.Handler
	file    string
	logs    *bytes.Buffer
}

func newFirstRun(t *testing.T) *firstRunRig {
	t.Helper()
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte(initialPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &firstRunRig{store: &firstRun{memStore: newMemStore()}, file: file, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	r.setup = &identity.Setup{Store: r.store, Logger: log}
	mux := http.NewServeMux()
	(&identity.InitialAdmin{PasswordFile: file, Setup: r.setup, Store: r.store, Logger: log, Now: func() time.Time { return now }}).Register(mux)
	r.handler = mux
	return r
}

func (r *firstRunRig) signIn(t *testing.T, password string, header bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost:8090/auth/initial-admin", strings.NewReader(`{"password":"`+password+`"}`))
	if header {
		req.Header.Set(identity.RequestHeader, "1")
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// The initial admin password signs in as an admin until an admin signs in
// with an account or through OIDC. Then neither the password nor the sessions
// it started sign anyone in, and the record says when it was retired.
func TestInitialAdmin_RetiredWhenAnAdminSignsIn(t *testing.T) {
	r := newFirstRun(t)
	// Space around it is from copying it.
	rec := r.signIn(t, ` `+initialPassword+`\n`, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign-in = %d %q", rec.Code, rec.Body)
	}
	first := cookieNamed(t, rec, identity.SessionCookie)
	d := &identity.Directory{Sessions: r.store, Access: bindings, Setup: r.setup, Now: func() time.Time { return now }}
	identify := func(c *http.Cookie) (identity.Identity, error) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
		req.AddCookie(c)
		return d.Identify(req)
	}
	if who, err := identify(first); err != nil || who.Username != identity.InitialAdminUser || who.Role != config.RoleAdmin || !who.InitialAdmin {
		t.Fatalf("identity = %+v, %v; want the initial admin", who, err)
	}
	if open, _ := r.setup.Open(t.Context()); !open {
		t.Fatal("signing in with the initial admin password finished setup")
	}

	// root@example.com is an admin through the bindings.
	r.store.sessions["root"] = store.Session{Subject: "sub-root", Username: "root@example.com", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if who, err := identify(&http.Cookie{Name: identity.SessionCookie, Value: "root", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}); err != nil || who.Role != config.RoleAdmin {
		t.Fatalf("root = %+v, %v", who, err)
	}
	if who, err := identify(first); !errors.Is(err, identity.ErrSignedOut) {
		t.Errorf("the initial admin's session once retired = %+v, %v; want signed out", who, err)
	}
	if rec := r.signIn(t, initialPassword, true); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "retired") {
		t.Errorf("the password once retired = %d %q, want 403 saying it is retired", rec.Code, rec.Body)
	}
	logs := r.logs.String()
	if strings.Count(logs, "signed in with the initial admin password") != 1 || strings.Count(logs, "initial admin password retired") != 1 || !strings.Contains(logs, "by=root@example.com") {
		t.Errorf("logs = %s, want one sign-in and one retirement by root", logs)
	}
	if strings.Contains(logs, initialPassword) {
		t.Error("the logs hold the initial admin password")
	}
	asked := r.store.asked
	_, _ = r.setup.Open(t.Context())
	if r.store.asked != asked {
		t.Error("Themis asks the store again after setup finished; setup never reopens")
	}
}

func TestInitialAdmin_Refuses(t *testing.T) {
	locked := now.Add(15 * time.Minute)
	for name, tc := range map[string]struct {
		password  string
		noHeader  bool
		prepare   func(*firstRunRig)
		code      int
		says      string
		failures  int
		lockedTil time.Time
	}{
		// A form on another site can post to localhost, but can't add the header.
		"without the request header": {password: initialPassword, noHeader: true, code: http.StatusForbidden, says: "Themis page"},
		"a wrong password":           {password: "a guess", code: http.StatusUnauthorized, says: "Wrong password", failures: 1},
		"the tenth wrong password": {
			password: "a guess", prepare: func(r *firstRunRig) { r.store.failures = 9 },
			code: http.StatusUnauthorized, says: "Wrong password", lockedTil: locked,
		},
		"the right one while locked": {
			password: initialPassword, prepare: func(r *firstRunRig) { r.store.lockedUntil = locked },
			code: http.StatusUnauthorized, says: "locked after 10 wrong tries, until 09:15 UTC", lockedTil: locked,
		},
		"once setup finished": {password: initialPassword, prepare: func(r *firstRunRig) { r.store.finished = true }, code: http.StatusForbidden, says: "retired"},
		"the database down": {
			password: initialPassword, prepare: func(r *firstRunRig) { r.store.err = errors.New("database down") },
			code: http.StatusServiceUnavailable, says: "can't sign you in right now",
		},
		"no password to read": {
			password: initialPassword, prepare: func(r *firstRunRig) { _ = os.Remove(r.file) },
			code: http.StatusServiceUnavailable, says: "can't read the initial admin password",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newFirstRun(t)
			if tc.prepare != nil {
				tc.prepare(r)
			}
			rec := r.signIn(t, tc.password, !tc.noHeader)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.says) {
				t.Errorf("sign-in = %d %q, want %d saying %q", rec.Code, rec.Body, tc.code, tc.says)
			}
			if len(r.store.sessions) != 0 {
				t.Errorf("a session was started: %v", r.store.sessions)
			}
			if r.store.failures != tc.failures || !r.store.lockedUntil.Equal(tc.lockedTil) {
				t.Errorf("wrong tries = %d, locked until %s; want %d, %s", r.store.failures, r.store.lockedUntil, tc.failures, tc.lockedTil)
			}
			if strings.Contains(r.logs.String(), tc.password) {
				t.Errorf("the logs hold the password tried: %s", r.logs)
			}
		})
	}
}
