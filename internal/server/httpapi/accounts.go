package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

// Accounts keeps the accounts people sign in to with a password, and the
// record of changes to them.
type Accounts interface {
	identity.PasswordStore
	Accounts(ctx context.Context) ([]store.Account, error)
	HasAccounts(ctx context.Context) (bool, error)
	CreateAccount(ctx context.Context, a store.Account) error
	SetPassword(ctx context.Context, username, hash string, temporary bool, c store.AccountChange) error
	SetRole(ctx context.Context, username, role string, c store.AccountChange) error
	DeleteAccount(ctx context.Context, username string, c store.AccountChange) error
	AccountEvents(ctx context.Context) ([]store.AccountEvent, error)
}

func (a *API) registerAccounts(mux *http.ServeMux) {
	mux.Handle("GET /v1/accounts", a.authed(config.RoleAdmin, a.listAccounts, nil))
	mux.Handle("POST /v1/accounts", a.authed(config.RoleAdmin, a.createAccount, nil))
	mux.Handle("PATCH /v1/accounts/{username}", a.authed(config.RoleAdmin, a.changeAccount, nil))
	mux.Handle("DELETE /v1/accounts/{username}", a.authed(config.RoleAdmin, a.deleteAccount, nil))
	mux.Handle("GET /v1/audit/accounts", a.authed(config.RoleAdmin, a.accountRecord, nil))
	mux.Handle("POST /v1/me/password", a.whileTemporary(a.changeOwnPassword))
}

type accountView struct {
	Username          string    `json:"username"`
	Role              string    `json:"role"`
	Locked            bool      `json:"locked"`
	TemporaryPassword bool      `json:"temporaryPassword"`
	CreatedAt         time.Time `json:"createdAt"`
	CreatedBy         string    `json:"createdBy"`
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Accounts.Accounts(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]accountView, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, accountView{
			Username: acc.Username, Role: acc.Role, Locked: acc.LockedUntil.After(a.now()), TemporaryPassword: acc.TemporaryPassword,
			CreatedAt: acc.CreatedAt, CreatedBy: acc.CreatedBy,
		})
	}
	a.reply(w, r, out)
}

type accountEventView struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	// Username is the account the event is about.
	Username string `json:"username,omitempty"`
	// Role is the role given, when one is.
	Role string `json:"role,omitempty"`
}

// accountRecord is the record of changes to accounts and of the initial
// admin password, newest first.
func (a *API) accountRecord(w http.ResponseWriter, r *http.Request) {
	events, err := a.Accounts.AccountEvents(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]accountEventView, 0, len(events))
	for _, e := range events {
		out = append(out, accountEventView(e))
	}
	a.reply(w, r, map[string][]accountEventView{"events": out})
}

// createAccount makes an account with a temporary password: an admin chose
// it, so its holder must choose their own at the first sign-in.
func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !a.decode(w, r, &in) {
		return
	}
	if err := errors.Join(identity.CheckUsername(in.Username), identity.CheckRole(in.Role), identity.CheckNewPassword(in.Username, in.Password)); err != nil {
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	}
	hash, err := identity.HashPassword(r.Context(), in.Password)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	err = a.Accounts.CreateAccount(r.Context(), store.Account{Username: in.Username, PasswordHash: hash, Role: in.Role, TemporaryPassword: true, CreatedAt: a.now(), CreatedBy: actor(r)})
	if errors.Is(err, store.ErrAccountExists) {
		http.Error(w, "There's already an account called "+in.Username+".", http.StatusConflict)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.logger().InfoContext(r.Context(), "an account was created", "username", in.Username, "role", in.Role, "by", actor(r))
	w.WriteHeader(http.StatusCreated)
}

// changeAccount sets an account's role, or a temporary password that ends
// its sessions. An admin can't change their own role, so no one locks
// themselves out by accident, and the last admin keeps theirs.
func (a *API) changeAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var in struct {
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !a.decode(w, r, &in) {
		return
	}
	if in.Role == "" && in.Password == "" {
		http.Error(w, "Say what to change: the role, the password, or both.", http.StatusBadRequest)
		return
	}
	if in.Role != "" {
		if err := identity.CheckRole(in.Role); err != nil {
			http.Error(w, sentence(err), http.StatusBadRequest)
			return
		}
		if self(r, username) {
			http.Error(w, "You can't change your own role. Ask another admin.", http.StatusForbidden)
			return
		}
	}
	if in.Password != "" {
		if err := identity.CheckNewPassword(username, in.Password); err != nil {
			http.Error(w, sentence(err), http.StatusBadRequest)
			return
		}
	}
	if _, err := a.Accounts.Account(r.Context(), username); err != nil {
		a.failAccount(w, r, username, err)
		return
	}
	// The role first: a refusal there changes nothing.
	if in.Role != "" {
		if err := a.Accounts.SetRole(r.Context(), username, in.Role, a.change(r)); err != nil {
			a.failAccount(w, r, username, err)
			return
		}
	}
	if in.Password != "" && !a.setPassword(w, r, username, in.Password, true) {
		return
	}
	a.logger().InfoContext(r.Context(), "an account was changed", "username", username, "role", in.Role, "temporary_password_set", in.Password != "", "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if self(r, username) {
		http.Error(w, "You can't delete your own account. Ask another admin.", http.StatusForbidden)
		return
	}
	if err := a.Accounts.DeleteAccount(r.Context(), username, a.change(r)); err != nil {
		a.failAccount(w, r, username, err)
		return
	}
	a.logger().InfoContext(r.Context(), "an account was deleted", "username", username, "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// changeOwnPassword lets someone signed in with an account choose its
// password, given the current one, which may be a temporary one.
func (a *API) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	who, _ := identity.From(r.Context())
	if !who.Account {
		http.Error(w, "Only people who sign in with a Themis account have a password here.", http.StatusBadRequest)
		return
	}
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !a.decode(w, r, &in) {
		return
	}
	// The same lock as signing in, so a stolen session can't be used to
	// guess the password.
	_, err := identity.Verify(r.Context(), a.Accounts, who.Username, in.Current, a.now())
	switch {
	case errors.Is(err, identity.ErrWrongPassword):
		http.Error(w, "Your current password isn't right. After 10 wrong tries your account is locked for 15 minutes.", http.StatusForbidden)
		return
	case errors.Is(err, identity.ErrAccountLocked):
		http.Error(w, "Your account is locked after too many wrong passwords. Try again in 15 minutes.", http.StatusForbidden)
		return
	case err != nil:
		a.failAccount(w, r, who.Username, err)
		return
	}
	if in.New == in.Current {
		http.Error(w, "Refused: the new password is the current one.", http.StatusBadRequest)
		return
	}
	if err := identity.CheckNewPassword(who.Username, in.New); err != nil {
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	}
	if !a.setPassword(w, r, who.Username, in.New, false) {
		return
	}
	a.logger().InfoContext(r.Context(), "a person changed their password", "username", who.Username)
	w.WriteHeader(http.StatusNoContent)
}

// setPassword hashes and stores password, which ends username's sessions.
func (a *API) setPassword(w http.ResponseWriter, r *http.Request, username, password string, temporary bool) bool {
	hash, err := identity.HashPassword(r.Context(), password)
	if err != nil {
		a.fail(w, r, err)
		return false
	}
	if err := a.Accounts.SetPassword(r.Context(), username, hash, temporary, a.change(r)); err != nil {
		a.failAccount(w, r, username, err)
		return false
	}
	return true
}

// change is who is changing an account, now. An admin account must remain
// unless the identity provider can sign an admin in: the break-glass token is
// for scripts and failed sign-ins, and the initial admin password retires.
func (a *API) change(r *http.Request) store.AccountChange {
	adminElsewhere := a.OIDC && slices.ContainsFunc(a.Access, func(b config.AccessBinding) bool { return b.Role == config.RoleAdmin })
	return store.AccountChange{By: actor(r), At: a.now(), KeepAnAdmin: !adminElsewhere}
}

func (a *API) failAccount(w http.ResponseWriter, r *http.Request, username string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "No such account.", http.StatusNotFound)
	case errors.Is(err, store.ErrLastAdmin):
		http.Error(w, "Refused: "+username+" is the last admin.", http.StatusConflict)
	default:
		a.fail(w, r, err)
	}
}

// self reports whether the account named username is the caller's own.
func self(r *http.Request, username string) bool {
	who, _ := identity.From(r.Context())
	return who.Account && who.Username == username
}

// sentence turns errors.Join's lines, written as Go errors, into sentences.
func sentence(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for i, l := range lines {
		lines[i] = strings.ToUpper(l[:1]) + l[1:] + "."
	}
	return strings.Join(lines, " ")
}
