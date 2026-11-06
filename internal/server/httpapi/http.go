// Package httpapi serves the HTTP API behind the UI: incidents, and the
// approval or denial of the actions awaiting a human.
package httpapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/server/modeltest"
	"github.com/hzerrad/themis/internal/server/statuspage"
	"github.com/hzerrad/themis/internal/store"
)

const maxBodyBytes = 64 << 10

type API struct {
	Store *store.Store
	// TokenFile holds the admin token, which acts as an admin.
	TokenFile string
	// Access gives people signed in through OIDC their roles.
	Access []config.AccessBinding
	// Sessions holds people signed in, through OIDC or with an account.
	Sessions identity.Sessions
	// OIDC says whether people can sign in through an identity provider.
	OIDC bool
	// Accounts keeps the accounts people sign in to with a password. Nil
	// means no one can.
	Accounts Accounts
	Logger   *slog.Logger
	Now      func() time.Time
	// Remedy says what Themis couldn't see and how to let it.
	Remedy hint.Writer
	// Suggestions takes fixes people suggest. Without it, they can't.
	Suggestions Suggestions
	// Fixes are the fixes Themis can carry out, which show what an action
	// changes and how a person suggests one.
	Fixes capability.Registry
	// CheckList is the checks Themis runs, which say what each answer means.
	CheckList check.List
	// RootCauses are what each pack's investigations may name as a cause,
	// which say what each means.
	RootCauses catalog.RootCauses
	// Config is Themis's live configuration, for the alerts its profiles
	// handle and the fixes they offer.
	Config config.Provider
	// Intake is where Alertmanager reaches Themis, and Namespace Themis's own,
	// for the commands Themis shows.
	Intake    config.Intake
	Namespace string
	// Settings are the model settings made in Themis's setup. Nil means
	// setup can't change them.
	Settings ModelSettings
	// ModelTests test the model's connection through the investigator. Nil
	// means no test is offered.
	ModelTests *modeltest.Tests
	// Checks say whether each part of the install works. Nil means the
	// status page isn't served.
	Checks interface {
		Checks(ctx context.Context) []statuspage.Check
	}
	// Setup is the install's first run, which creating the first admin
	// account finishes, as an admin signing in with an account and a password
	// of their own does. Nil means no one signs in with the initial admin
	// password.
	Setup *identity.Setup
	// InitialAdmin is the password that signs in to make the first admin
	// account, which the sign-in page offers. Nil means there is none.
	InitialAdmin *config.InitialAdmin
	// Gate keeps the API closed but for making the first admin account while
	// none exists. Nil means always open.
	Gate *identity.Gate
}

// What a route serves beyond someone signed in with the role it needs.
type serves struct {
	// temporary: someone whose password is temporary.
	temporary bool
	// closed: anyone, while no admin account exists.
	closed bool
	// initialAdmin: whoever signed in with the initial admin password.
	initialAdmin bool
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sign-in", a.signInMethods)
	mux.Handle("GET /v1/me", a.guard("", serves{temporary: true, closed: true, initialAdmin: true}, a.me, nil))
	mux.Handle("GET /v1/incidents", a.authed(config.RoleViewer, a.list, nil))
	mux.Handle("GET /v1/incidents/{id}", a.authed(config.RoleViewer, a.detail, incidentFields))
	mux.Handle("POST /v1/incidents/{id}/proposals", a.authed(config.RoleApprover, a.suggest, incidentFields))
	mux.Handle("POST /v1/actions/{id}/approve", a.authed(config.RoleApprover, a.approve, a.actionFields))
	mux.Handle("POST /v1/actions/{id}/deny", a.authed(config.RoleApprover, a.deny, a.actionFields))
	if a.Accounts != nil {
		a.registerAccounts(mux)
	}
	if a.Checks != nil {
		mux.Handle("GET /v1/status", a.authed(config.RoleViewer, a.status, nil))
	}
	if a.Config != nil {
		mux.Handle("GET /v1/setup/alertmanager", a.authed(config.RoleAdmin, a.alertmanagerSetup, nil))
	}
	if a.Settings != nil {
		mux.Handle("GET /v1/settings/model", a.authed(config.RoleAdmin, a.modelSettings, nil))
		mux.Handle("PUT /v1/settings/model", a.authed(config.RoleAdmin, a.setModelSettings, nil))
		if a.ModelTests != nil {
			mux.Handle("POST /v1/settings/model/test", a.authed(config.RoleAdmin, a.testModel, nil))
		}
	}
}

// status says whether each part of the install works.
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	a.reply(w, r, map[string][]statuspage.Check{"checks": a.Checks.Checks(r.Context())})
}

// authed serves h to whoever has role, or to anyone signed in when role is
// empty, once an admin account exists. Someone whose password is temporary,
// or who signed in with the initial admin password, is refused.
func (a *API) authed(role string, h http.HandlerFunc, fields func(*http.Request) []slog.Attr) http.Handler {
	return a.guard(role, serves{}, h, fields)
}

// initialAdminOnly is all the initial admin password is refused with once an
// admin account exists.
const initialAdminOnly = "Refused. The initial admin password only creates the first admin account."

// guard serves h to whoever has role and s allows, with the log fields of
// the request's path, and logs the request at debug level.
func (a *API) guard(role string, s serves, h http.HandlerFunc, fields func(*http.Request) []slog.Attr) http.Handler {
	directory := &identity.Directory{TokenFile: a.TokenFile, Access: a.Access, Sessions: a.Sessions, Setup: a.Setup, Now: a.now}
	if a.Accounts != nil {
		directory.Accounts = a.Accounts
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &statusWriter{ResponseWriter: rw, status: http.StatusOK}
		defer func() {
			a.logger().DebugContext(r.Context(), "API request served", "route", r.Pattern, "status", w.status)
		}()
		who, err := directory.Identify(r)
		switch {
		case errors.Is(err, identity.ErrUnavailable):
			a.logger().ErrorContext(r.Context(), "cannot check who sent a request", telemetry.FieldError, err)
			http.Error(w, "Themis can't check sign-ins right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		case errors.Is(err, identity.ErrNoRequestHeader):
			http.Error(w, "Make changes from the Themis page.", http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, "Sign in to Themis first.", http.StatusUnauthorized)
			return
		case who.TemporaryPassword && !s.temporary:
			http.Error(w, "Refused. The password is temporary. Choose a new one first.", http.StatusForbidden)
			return
		}
		if !s.closed {
			open, err := a.Gate.Open(r.Context())
			if err != nil {
				a.fail(w, r, err)
				return
			}
			if !open {
				http.Error(w, identity.ClosedMessage, http.StatusConflict)
				return
			}
		}
		if who.InitialAdmin && !s.initialAdmin {
			http.Error(w, initialAdminOnly, http.StatusForbidden)
			return
		}
		if role != "" && !who.Can(role) {
			if who.Role == "" {
				http.Error(w, "You're signed in as "+who.Username+", but you don't have access to Themis yet. Ask a Themis admin to add you.", http.StatusForbidden)
			} else {
				http.Error(w, "This needs the "+role+" role. You have the "+who.Role+" role.", http.StatusForbidden)
			}
			return
		}
		ctx := identity.WithIdentity(r.Context(), who)
		if fields != nil {
			ctx = telemetry.WithAttrs(ctx, fields(r)...)
		}
		// The deferred log reads r, so it carries the fields too.
		r = r.WithContext(ctx)
		h(w, r)
	})
}

// actor is who to record as having done something.
func actor(r *http.Request) string {
	who, _ := identity.From(r.Context())
	return who.Username
}

type meView struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups,omitempty"`
	// Role is empty when the person has no access yet.
	Role string `json:"role"`
	// Token is set for the shared admin token, which proves the token, not who.
	Token bool `json:"token,omitempty"`
	// InitialAdmin is set for whoever signed in with the initial admin
	// password, which proves the password, not who.
	InitialAdmin bool `json:"initialAdmin,omitempty"`
	// Account is set for someone signed in with a Themis account.
	Account bool `json:"account,omitempty"`
	// TemporaryPassword is set while that account's password is one an admin
	// set, which its holder must replace before anything else.
	TemporaryPassword bool `json:"temporaryPassword,omitempty"`
	// Closed is set while no admin account exists, and Themis serves nothing
	// but the making of the first.
	Closed bool `json:"closed,omitempty"`
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	who, _ := identity.From(r.Context())
	open, err := a.Gate.Open(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, meView{
		Username: who.Username, Groups: who.Groups, Role: who.Role, Token: who.Token, InitialAdmin: who.InitialAdmin, Account: who.Account,
		TemporaryPassword: who.TemporaryPassword, Closed: !open,
	})
}

type signInView struct {
	OIDC     bool `json:"oidc"`
	Password bool `json:"password"`
	Token    bool `json:"token"`
	// InitialAdmin is set while the initial admin password signs in.
	InitialAdmin *initialAdminView `json:"initialAdmin,omitzero"`
}

type initialAdminView struct {
	// Command prints the password, when Themis knows its Secret.
	Command string `json:"command,omitempty"`
}

// signInMethods tells the UI how people sign in, before anyone has.
func (a *API) signInMethods(w http.ResponseWriter, r *http.Request) {
	v := signInView{OIDC: a.OIDC, Token: true}
	if a.Accounts != nil {
		var err error
		if v.Password, err = a.Accounts.HasAccounts(r.Context()); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	// The initial admin password only makes the first admin account.
	if a.InitialAdmin != nil && a.Setup != nil {
		setup, err := a.Setup.Open(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		accounts, err := a.Gate.Open(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if setup && !accounts {
			v.InitialAdmin = &initialAdminView{}
			if a.InitialAdmin.Secret != "" && a.Namespace != "" {
				v.InitialAdmin.Command = fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.data.password}' | base64 -d", a.Namespace, a.InitialAdmin.Secret)
			}
		}
	}
	a.reply(w, r, v)
}

func incidentFields(r *http.Request) []slog.Attr {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		return nil
	}
	return []slog.Attr{slog.String(telemetry.FieldIncidentID, id)}
}

func (a *API) actionFields(r *http.Request) []slog.Attr {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		return nil
	}
	attrs := []slog.Attr{slog.String(telemetry.FieldActionID, id)}
	if act, err := a.Store.Action(r.Context(), a.Store.DB(), id); err == nil {
		attrs = append(attrs, slog.String(telemetry.FieldIncidentID, act.IncidentID))
	}
	return attrs
}

// statusWriter records the status of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *API) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxBodyBytes), v); err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (a *API) reply(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, v); err != nil {
		a.logger().ErrorContext(r.Context(), "cannot write a response", telemetry.FieldError, err)
	}
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "No such incident or fix.", http.StatusNotFound)
	case errors.Is(err, store.ErrDigestMismatch):
		http.Error(w, "This fix changed since you opened it. Reload the page and review it again.", http.StatusConflict)
	case errors.Is(err, store.ErrAlreadyDecided), errors.Is(err, store.ErrStateConflict):
		http.Error(w, "This fix was already approved, denied or canceled.", http.StatusConflict)
	case errors.Is(err, store.ErrExpired):
		http.Error(w, "Approval expired. This fix can no longer be approved.", http.StatusGone)
	default:
		a.logger().ErrorContext(r.Context(), "API request failed", telemetry.FieldError, err)
		http.Error(w, "Something went wrong in Themis. The core's logs have the details.", http.StatusInternalServerError)
	}
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}
