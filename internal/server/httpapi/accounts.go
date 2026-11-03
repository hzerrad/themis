package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/identity"
	"github.com/hzerrad/themis/internal/store"
)

// Accounts keeps the accounts people sign in to with a password.
type Accounts interface {
	identity.Accounts
	Accounts(ctx context.Context) ([]store.Account, error)
	HasAccounts(ctx context.Context) (bool, error)
	CreateAccount(ctx context.Context, a store.Account) error
	SetPassword(ctx context.Context, username, hash string) error
	SetRole(ctx context.Context, username, role string) error
	DeleteAccount(ctx context.Context, username string) error
}

func (a *API) registerAccounts(mux *http.ServeMux) {
	mux.Handle("GET /v1/accounts", a.authed(config.RoleAdmin, a.listAccounts, nil))
	mux.Handle("POST /v1/accounts", a.authed(config.RoleAdmin, a.createAccount, nil))
	mux.Handle("PATCH /v1/accounts/{username}", a.authed(config.RoleAdmin, a.changeAccount, nil))
	mux.Handle("DELETE /v1/accounts/{username}", a.authed(config.RoleAdmin, a.deleteAccount, nil))
	mux.Handle("POST /v1/me/password", a.authed("", a.changeOwnPassword, nil))
}

type accountView struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Locked    bool      `json:"locked"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
}

func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Accounts.Accounts(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]accountView, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, accountView{Username: acc.Username, Role: acc.Role, Locked: acc.LockedUntil.After(a.now()), CreatedAt: acc.CreatedAt, CreatedBy: acc.CreatedBy})
	}
	a.reply(w, r, out)
}

func (a *API) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !a.decode(w, r, &in) {
		return
	}
	if err := errors.Join(identity.CheckUsername(in.Username), checkRole(in.Role), identity.CheckNewPassword(in.Username, in.Password)); err != nil {
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	}
	hash, err := identity.HashPassword(r.Context(), in.Password)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	err = a.Accounts.CreateAccount(r.Context(), store.Account{Username: in.Username, PasswordHash: hash, Role: in.Role, CreatedAt: a.now(), CreatedBy: actor(r)})
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

// changeAccount sets an account's role or password. An admin can't change
// their own role, so no one locks themselves out by accident.
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
		if err := checkRole(in.Role); err != nil {
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
		a.failAccount(w, r, err)
		return
	}
	if in.Password != "" && !a.setPassword(w, r, username, in.Password) {
		return
	}
	if in.Role != "" {
		if err := a.Accounts.SetRole(r.Context(), username, in.Role); err != nil {
			a.failAccount(w, r, err)
			return
		}
	}
	a.logger().InfoContext(r.Context(), "an account was changed", "username", username, "role", in.Role, "password_changed", in.Password != "", "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if self(r, username) {
		http.Error(w, "You can't delete your own account. Ask another admin.", http.StatusForbidden)
		return
	}
	if err := a.Accounts.DeleteAccount(r.Context(), username); err != nil {
		a.failAccount(w, r, err)
		return
	}
	a.logger().InfoContext(r.Context(), "an account was deleted", "username", username, "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// changeOwnPassword lets someone signed in with an account change its
// password, given the current one.
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
	acc, err := a.Accounts.Account(r.Context(), who.Username)
	if err != nil {
		a.failAccount(w, r, err)
		return
	}
	ok, err := identity.CheckPassword(r.Context(), acc.PasswordHash, in.Current)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !ok {
		http.Error(w, "Your current password isn't right.", http.StatusForbidden)
		return
	}
	if err := identity.CheckNewPassword(who.Username, in.New); err != nil {
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	}
	if !a.setPassword(w, r, who.Username, in.New) {
		return
	}
	a.logger().InfoContext(r.Context(), "a person changed their password", "username", who.Username)
	w.WriteHeader(http.StatusNoContent)
}

// setPassword hashes and stores password, which ends username's sessions.
func (a *API) setPassword(w http.ResponseWriter, r *http.Request, username, password string) bool {
	hash, err := identity.HashPassword(r.Context(), password)
	if err != nil {
		a.fail(w, r, err)
		return false
	}
	if err := a.Accounts.SetPassword(r.Context(), username, hash); err != nil {
		a.failAccount(w, r, err)
		return false
	}
	return true
}

func (a *API) failAccount(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "No such account.", http.StatusNotFound)
		return
	}
	a.fail(w, r, err)
}

func checkRole(role string) error {
	if !slices.Contains(config.Roles, role) {
		return errors.New("the role must be viewer, responder, approver or admin")
	}
	return nil
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
