package identity

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/store"
)

// Wrong passwords lock an account for a while, so no one can keep guessing.
const (
	maxWrongPasswords = 10
	lockFor           = 15 * time.Minute
	accountSessionTTL = 8 * time.Hour
)

// PasswordStore keeps accounts and counts their wrong passwords.
type PasswordStore interface {
	Account(ctx context.Context, username string) (store.Account, error)
	PasswordFailed(ctx context.Context, username string, now time.Time, maxFailures int, lockFor time.Duration) error
	PasswordSucceeded(ctx context.Context, username string) error
}

// AccountStore keeps accounts and the sessions they start.
type AccountStore interface {
	PasswordStore
	CreateSession(ctx context.Context, id string, se store.Session) error
}

// Verify's errors, besides store.ErrNotFound for an unknown username.
var (
	ErrWrongPassword = errors.New("wrong password")
	ErrAccountLocked = errors.New("account locked")
)

// Verify checks username's password. A wrong one counts towards locking the
// account and a right one clears the count, so every place that checks a
// password is covered by the same lock.
func Verify(ctx context.Context, s PasswordStore, username, password string, now time.Time) (store.Account, error) {
	a, err := s.Account(ctx, username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Take as long as a real check, so timing doesn't reveal the username.
		_, _ = CheckPassword(ctx, dummyHash(), password)
		return store.Account{}, err
	case err != nil:
		return store.Account{}, err
	case a.LockedUntil.After(now):
		return store.Account{}, ErrAccountLocked
	}
	ok, err := CheckPassword(ctx, a.PasswordHash, password)
	if err != nil {
		return store.Account{}, err
	}
	if !ok {
		if err := s.PasswordFailed(ctx, a.Username, now, maxWrongPasswords, lockFor); err != nil {
			return store.Account{}, err
		}
		return store.Account{}, ErrWrongPassword
	}
	if err := s.PasswordSucceeded(ctx, a.Username); err != nil {
		return store.Account{}, err
	}
	return a, nil
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
	a, err := Verify(ctx, p.Store, in.Username, in.Password, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		p.refuse(w, r, in.Username, "unknown username")
		return
	case errors.Is(err, ErrAccountLocked), errors.Is(err, ErrWrongPassword):
		p.refuse(w, r, in.Username, err.Error())
		return
	case err != nil:
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
			s.logger().ErrorContext(r.Context(), "cannot end a session", telemetry.FieldError, err)
			http.Error(w, "Themis can't sign you out right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		}
	}
	// The session cookie may have been set secure (a password, or OIDC over
	// HTTPS) or not (OIDC over plain HTTP). A browser ignores a secure
	// Set-Cookie sent over plain HTTP, but over HTTPS lets a non-secure one
	// replace a secure cookie, so a non-secure deletion clears either.
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode}) //nolint:gosec // deletes the cookie however it was set
	w.WriteHeader(http.StatusNoContent)
}

func (s *SignOut) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
