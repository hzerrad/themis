package people

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

// Wrong passwords lock an account for a while, so no one can keep guessing.
const (
	maxWrongPasswords = 10
	lockFor           = 15 * time.Minute
	accountSessionTTL = 8 * time.Hour
)

// AccountStore keeps accounts and the sessions they start.
type AccountStore interface {
	Account(ctx context.Context, username string) (store.Account, error)
	PasswordFailed(ctx context.Context, username string, now time.Time, maxFailures int, lockFor time.Duration) error
	PasswordSucceeded(ctx context.Context, username string) error
	CreateSession(ctx context.Context, id string, se store.Session) error
}

// Passwords signs people in with a Themis account's username and password.
type Passwords struct {
	Store  AccountStore
	Logger *slog.Logger
	Now    func() time.Time
}

// Register serves the password sign-in on mux.
func (p *Passwords) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/password", p.signIn)
}

// wrongPassword is all a failed sign-in says, so it can't tell anyone which
// usernames exist or which accounts are locked.
const wrongPassword = "That username and password don't match. After 10 wrong tries an account is locked for 15 minutes."

func (p *Passwords) signIn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Header.Get(RequestHeader) == "" {
		http.Error(w, "Sign in from the Themis page.", http.StatusForbidden)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 4<<10), &in); err != nil {
		http.Error(w, "Enter a username and password.", http.StatusBadRequest)
		return
	}
	now := p.now()
	a, err := p.Store.Account(ctx, in.Username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Take as long as a real check, so timing doesn't reveal the username.
		_, _ = CheckPassword(ctx, dummyHash(), in.Password)
		p.refuse(w, r, in.Username, "unknown username")
		return
	case err != nil:
		p.fail(w, r, err)
		return
	case a.LockedUntil.After(now):
		p.refuse(w, r, in.Username, "account locked")
		return
	}
	ok, err := CheckPassword(ctx, a.PasswordHash, in.Password)
	if err != nil {
		p.fail(w, r, err)
		return
	}
	if !ok {
		if err := p.Store.PasswordFailed(ctx, a.Username, now, maxWrongPasswords, lockFor); err != nil {
			p.fail(w, r, err)
			return
		}
		p.refuse(w, r, in.Username, "wrong password")
		return
	}
	if err := p.Store.PasswordSucceeded(ctx, a.Username); err != nil {
		p.fail(w, r, err)
		return
	}
	id := random()
	se := store.Session{Subject: store.AccountSubjectPrefix + a.Username, Username: a.Username, CreatedAt: now, ExpiresAt: now.Add(accountSessionTTL)}
	if err := p.Store.CreateSession(ctx, id, se); err != nil {
		p.fail(w, r, err)
		return
	}
	p.logger().InfoContext(ctx, "a person signed in", "username", a.Username, "with", "password")
	http.SetCookie(w, sessionCookie(r, id, accountSessionTTL))
	w.WriteHeader(http.StatusNoContent)
}

func (p *Passwords) refuse(w http.ResponseWriter, r *http.Request, username, why string) {
	p.logger().WarnContext(r.Context(), "a password sign-in failed", "username", username, "reason", why)
	http.Error(w, wrongPassword, http.StatusUnauthorized)
}

func (p *Passwords) fail(w http.ResponseWriter, r *http.Request, err error) {
	p.logger().ErrorContext(r.Context(), "cannot check a password", telemetry.FieldError, err)
	http.Error(w, "Themis can't sign you in right now. Try again shortly.", http.StatusServiceUnavailable)
}

func (p *Passwords) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Passwords) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// sessionCookie holds a password sign-in's session. It's secure everywhere
// but localhost: a password should never cross a network in the clear, and
// a secure cookie over plain HTTP is never sent back, so sign-in fails
// instead.
func sessionCookie(r *http.Request, id string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // secure everywhere but localhost
		Name: SessionCookie, Value: id, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: !loopbackHost(r.Host),
	}
}

// SignOut ends whatever session the browser holds, however it started.
type SignOut struct {
	Store interface {
		EndSession(ctx context.Context, id string) error
	}
	Logger *slog.Logger
}

// Register serves sign-out on mux.
func (s *SignOut) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/logout", s.logout)
}

func (s *SignOut) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(RequestHeader) == "" {
		http.Error(w, "Sign out from the Themis page.", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if err := s.Store.EndSession(r.Context(), c.Value); err != nil {
			s.Logger.ErrorContext(r.Context(), "cannot end a session", telemetry.FieldError, err)
			http.Error(w, "Themis can't sign you out right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: !loopbackHost(r.Host)}) //nolint:gosec // secure everywhere but localhost
	w.WriteHeader(http.StatusNoContent)
}
