package identity

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/platform/token"
	"github.com/hzerrad/themis/internal/store"
)

// InitialAdminUser is the username of whoever signs in with the initial admin
// password. It proves the password, not who.
const InitialAdminUser = "initial-admin"

// SetupUser named whoever came in through kubectl port-forward before setup
// finished, in versions of Themis that let them in as admin. Records still
// carry it.
const SetupUser = "setup"

// reserved reports whether username names one of Themis's own ways in, which
// records use, so no one may sign in as it.
func reserved(username string) bool {
	return username == TokenUser || username == InitialAdminUser || username == SetupUser || username == ChartUser
}

// SetupStore says whether setup has finished, and records when it does.
type SetupStore interface {
	SetupFinished(ctx context.Context) (bool, error)
	FinishSetup(ctx context.Context, by string, at time.Time) (bool, error)
}

// Setup is an install's first run. It is open until an admin signs in with a
// Themis account and a password of their own. While it is open, the initial
// admin password signs in to make the first admin account; once it
// finishes, the password and the sessions it started are retired for good.
type Setup struct {
	Store  SetupStore
	Logger *slog.Logger

	finished atomic.Bool
}

// Open reports whether setup is open.
func (s *Setup) Open(ctx context.Context) (bool, error) {
	if s.finished.Load() {
		return false, nil
	}
	finished, err := s.Store.SetupFinished(ctx)
	if err != nil {
		return false, err
	}
	// Setup never reopens, so once finished, it stops asking.
	s.finished.Store(finished)
	return !finished, nil
}

// finish ends setup once who is an admin signed in with an account and a
// password of their own: the initial admin password is then retired.
func (s *Setup) finish(ctx context.Context, who Identity, at time.Time) {
	if !who.Account || who.TemporaryPassword || who.Role != config.RoleAdmin || s.finished.Load() {
		return
	}
	done, err := s.Store.FinishSetup(ctx, who.Username, at)
	if err != nil {
		s.logger().ErrorContext(ctx, "cannot record that setup finished", telemetry.FieldError, err)
		return
	}
	s.finished.Store(true)
	if done {
		s.logger().InfoContext(ctx, "initial admin password retired", "by", who.Username)
	}
}

func (s *Setup) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// InitialAdminStore counts wrong initial admin passwords, keeps the sessions
// the right one starts, and records each.
type InitialAdminStore interface {
	InitialAdminLockedUntil(ctx context.Context) (time.Time, error)
	InitialAdminPasswordFailed(ctx context.Context, now time.Time, maxFailures int, lockFor time.Duration) error
	InitialAdminPasswordSucceeded(ctx context.Context) error
	RecordAccountEvent(ctx context.Context, e store.AccountEvent) error
	CreateSession(ctx context.Context, id string, se store.Session) error
}

// initialAdminSessionTTL is how long a sign-in with the initial admin
// password lasts: long enough to set up, short enough not to linger.
const initialAdminSessionTTL = time.Hour

// InitialAdmin signs in with the initial admin password while setup is open
// and no admin account exists: it only makes the first admin account. The
// password is read from PasswordFile at each try, compared in constant time,
// and never stored, logged or shown.
type InitialAdmin struct {
	PasswordFile string
	Setup        *Setup
	Gate         *Gate
	Store        InitialAdminStore
	Logger       *slog.Logger
	Now          func() time.Time
}

// Register serves the initial admin sign-in on mux.
func (i *InitialAdmin) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/initial-admin", i.signIn)
}

// What a refused sign-in says. The password is the only one of its kind, so
// saying it is locked or retired reveals nothing about anyone's account.
const (
	wrongInitialPassword   = "Wrong password. After 10 wrong tries in a row, the initial admin password is locked for 15 minutes."
	retiredInitialPassword = "The initial admin password is retired. Sign in with your account."
	adminAccountExists     = "An admin account exists. Sign in with it."
)

func (i *InitialAdmin) signIn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Header.Get(RequestHeader) == "" {
		http.Error(w, "Sign in from the Themis page.", http.StatusForbidden)
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 4<<10), &in); err != nil {
		http.Error(w, "Enter the initial admin password.", http.StatusBadRequest)
		return
	}
	now := i.now()
	open, err := i.Setup.Open(ctx)
	if err != nil {
		i.fail(w, r, err)
		return
	}
	if !open {
		i.refuse(w, r, "retired", http.StatusForbidden, retiredInitialPassword)
		return
	}
	// Its only use is making the first admin account.
	if accounts, err := i.Gate.Open(ctx); err != nil {
		i.fail(w, r, err)
		return
	} else if accounts {
		i.refuse(w, r, "an admin account exists", http.StatusForbidden, adminAccountExists)
		return
	}
	until, err := i.Store.InitialAdminLockedUntil(ctx)
	if err != nil {
		i.fail(w, r, err)
		return
	}
	if until.After(now) {
		i.refuse(w, r, "locked", http.StatusUnauthorized, fmt.Sprintf(
			"The initial admin password is locked after 10 wrong tries, until %s UTC. The break-glass token still signs in.", until.UTC().Format("15:04")))
		return
	}
	want, err := token.Read(i.PasswordFile)
	if err != nil {
		i.logger().ErrorContext(ctx, "cannot read the initial admin password", telemetry.FieldError, err)
		http.Error(w, "Themis can't read the initial admin password. The core's logs have the details.", http.StatusServiceUnavailable)
		return
	}
	// The password is random letters and digits: space around it is from
	// copying it.
	same, err := samePassword(ctx, strings.TrimSpace(in.Password), want)
	if err != nil {
		i.fail(w, r, err)
		return
	}
	if !same {
		if err := i.Store.InitialAdminPasswordFailed(ctx, now, maxWrongPasswords, lockFor); err != nil {
			i.fail(w, r, err)
			return
		}
		i.refuse(w, r, "wrong password", http.StatusUnauthorized, wrongInitialPassword)
		return
	}
	if err := i.Store.InitialAdminPasswordSucceeded(ctx); err != nil {
		i.fail(w, r, err)
		return
	}
	// On the record before the session starts, so no use of it goes unrecorded.
	if err := i.Store.RecordAccountEvent(ctx, store.AccountEvent{At: now, Actor: InitialAdminUser, Action: store.InitialAdminSignedIn}); err != nil {
		i.fail(w, r, err)
		return
	}
	id := random()
	se := store.Session{Subject: store.InitialAdminSubject, Username: InitialAdminUser, CreatedAt: now, ExpiresAt: now.Add(initialAdminSessionTTL)}
	if err := i.Store.CreateSession(ctx, id, se); err != nil {
		i.fail(w, r, err)
		return
	}
	i.logger().InfoContext(ctx, "signed in with the initial admin password")
	http.SetCookie(w, sessionCookie(r, id, initialAdminSessionTTL))
	w.WriteHeader(http.StatusNoContent)
}

// samePassword checks got through argon2id, as account passwords are
// checked: in constant time, and without the length showing.
func samePassword(ctx context.Context, got string, want []byte) (bool, error) {
	hash, err := HashPassword(ctx, string(want))
	if err != nil {
		return false, err
	}
	return CheckPassword(ctx, hash, got)
}

func (i *InitialAdmin) refuse(w http.ResponseWriter, r *http.Request, why string, code int, message string) {
	i.logger().WarnContext(r.Context(), "an initial admin password sign-in failed", "reason", why)
	http.Error(w, message, code)
}

func (i *InitialAdmin) fail(w http.ResponseWriter, r *http.Request, err error) {
	i.logger().ErrorContext(r.Context(), "cannot check the initial admin password", telemetry.FieldError, err)
	http.Error(w, "Themis can't sign you in right now. Try again shortly.", http.StatusServiceUnavailable)
}

func (i *InitialAdmin) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *InitialAdmin) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}
