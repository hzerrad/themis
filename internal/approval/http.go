// Package approval serves the HTTP API behind the UI: incidents, and the
// approval or denial of the actions awaiting a human.
package approval

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/checks"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/people"
	"github.com/hzerrad/themis/internal/remedy"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const maxBodyBytes = 64 << 10

type API struct {
	Store *store.Store
	// TokenFile holds the admin token, which acts as an admin.
	TokenFile string
	// Access gives people signed in through OIDC their roles.
	Access []config.AccessBinding
	// Sessions holds people signed in, through OIDC or with an account.
	Sessions people.Sessions
	// OIDC says whether people can sign in through an identity provider.
	OIDC bool
	// Accounts keeps the accounts people sign in to with a password. Nil
	// means no one can.
	Accounts Accounts
	Logger   *slog.Logger
	Now      func() time.Time
	// Remedy says what Themis couldn't see and how to let it.
	Remedy remedy.Writer
	// Suggestions takes fixes people suggest. Without it, they can't.
	Suggestions Suggestions
	// Config is Themis's live configuration, for the alerts its profiles
	// handle.
	Config config.Provider
	// Intake is where Alertmanager reaches Themis, and Namespace Themis's own,
	// for the setup steps Themis shows.
	Intake    config.Intake
	Namespace string
	// Checks say whether each part of the install works. Nil means the
	// status page isn't served.
	Checks interface {
		Checks(ctx context.Context) []checks.Check
	}
	// Setup records when the first admin signs in for real, which closes the
	// setup door. Nil means there is no door.
	Setup people.SetupStore

	setupFinished atomic.Bool
}

// Suggestions takes a fix suggested by a person. The core checks it exactly
// as it checks the investigator's.
type Suggestions interface {
	// SubmitFromPerson takes a suggestion from person, or from the holder of
	// the admin token when person is empty.
	SubmitFromPerson(ctx context.Context, person string, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error)
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sign-in", a.signInMethods)
	mux.Handle("GET /v1/me", a.authed("", a.me, nil))
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
}

// status says whether each part of the install works.
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	a.reply(w, r, map[string][]checks.Check{"checks": a.Checks.Checks(r.Context())})
}

// authed serves h to whoever has role, or to anyone signed in when role is
// empty, with the log fields of the request's path, and logs the request at
// debug level.
func (a *API) authed(role string, h http.HandlerFunc, fields func(*http.Request) []slog.Attr) http.Handler {
	directory := &people.Directory{TokenFile: a.TokenFile, Access: a.Access, Sessions: a.Sessions, Now: a.now}
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
		case errors.Is(err, people.ErrUnavailable):
			a.logger().ErrorContext(r.Context(), "cannot check who sent a request", telemetry.FieldError, err)
			http.Error(w, "Themis can't check sign-ins right now. Try again shortly.", http.StatusServiceUnavailable)
			return
		case errors.Is(err, people.ErrNoRequestHeader):
			http.Error(w, "Make changes from the Themis page.", http.StatusForbidden)
			return
		case err != nil:
			http.Error(w, "Sign in to Themis first.", http.StatusUnauthorized)
			return
		}
		a.finishSetup(r.Context(), who)
		if role != "" && !who.Can(role) {
			if who.Role == "" {
				http.Error(w, "You're signed in as "+who.Username+", but you don't have access to Themis yet. Ask a Themis admin to add you.", http.StatusForbidden)
			} else {
				http.Error(w, "This needs the "+role+" role. You have the "+who.Role+" role.", http.StatusForbidden)
			}
			return
		}
		ctx := people.WithIdentity(r.Context(), who)
		if fields != nil {
			ctx = telemetry.WithAttrs(ctx, fields(r)...)
		}
		// The deferred log reads r, so it carries the fields too.
		r = r.WithContext(ctx)
		h(w, r)
	})
}

// finishSetup closes the setup door once an admin has signed in through a
// real sign-in, since someone can now sign in without kubectl.
func (a *API) finishSetup(ctx context.Context, who people.Identity) {
	if a.Setup == nil || who.Token || who.Setup || who.Role != config.RoleAdmin || a.setupFinished.Load() {
		return
	}
	if err := a.Setup.FinishSetup(ctx, who.Username, a.now()); err != nil {
		a.logger().ErrorContext(ctx, "cannot record that setup finished", telemetry.FieldError, err)
		return
	}
	a.setupFinished.Store(true)
	a.logger().InfoContext(ctx, "setup finished: an admin signed in, so kubectl port-forward no longer lets anyone in as admin", "user", who.Username)
}

// actor is who to record as having done something.
func actor(r *http.Request) string {
	who, _ := people.From(r.Context())
	return who.Username
}

type meView struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups,omitempty"`
	// Role is empty when the person has no access yet.
	Role string `json:"role"`
	// Token is set for the shared admin token, which proves the token, not who.
	Token bool `json:"token,omitempty"`
	// Setup is set for whoever came in through kubectl port-forward before
	// setup finished.
	Setup bool `json:"setup,omitempty"`
	// Account is set for someone signed in with a Themis account.
	Account bool `json:"account,omitempty"`
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	who, _ := people.From(r.Context())
	a.reply(w, r, meView{Username: who.Username, Groups: who.Groups, Role: who.Role, Token: who.Token, Setup: who.Setup, Account: who.Account})
}

// signInMethods tells the UI how people sign in, before anyone has.
func (a *API) signInMethods(w http.ResponseWriter, r *http.Request) {
	password := false
	if a.Accounts != nil {
		var err error
		if password, err = a.Accounts.HasAccounts(r.Context()); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.reply(w, r, map[string]bool{"oidc": a.OIDC, "password": password, "token": true})
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

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	incidents, err := a.Store.ListIncidents(r.Context(), 100)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]incidentView, 0, len(incidents))
	for _, in := range incidents {
		out = append(out, viewIncident(in))
	}
	a.reply(w, r, out)
}

func (a *API) detail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := a.Store.Incident(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := detailView{Incident: viewIncident(in)}
	if d.Transitions, err = a.Store.Transitions(ctx, in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := a.Store.Evidence(ctx, in.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Raw evidence, such as logs, is for responders.
	who, _ := people.From(ctx)
	for _, it := range items {
		v := evidenceView{Item: it}
		if who.Can(config.RoleResponder) {
			v.Data = it.Data
		}
		d.Evidence = append(d.Evidence, v)
	}
	if d.Predicates, err = a.Store.Predicates(ctx, in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if inv, err := a.Store.IncidentInvestigation(ctx, in.ID); err == nil {
		d.Investigation = &inv
		// Once someone's suggestion moves it on, what's next matters more.
		if inv.Conclusion == "inconclusive" && (in.State == incident.Inconclusive || in.State == incident.ClosedNoAction) {
			d.Headline = remedy.Headline(inv.Blind)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	gaps, err := a.Store.EvidenceGaps(ctx, in.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d.Missing = a.Remedy.Messages(in, gaps)
	if d.Proposals, err = a.Store.Proposals(ctx, a.Store.DB(), in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	actions, err := a.Store.IncidentActions(ctx, a.Store.DB(), in.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, act := range actions {
		v := viewAction(act)
		if e, err := a.Store.ActionExecution(ctx, a.Store.DB(), act.ID); err == nil {
			v.Execution = &e
		}
		if ver, err := a.Store.Verification(ctx, a.Store.DB(), act.ID); err == nil {
			v.Verification = &verificationView{Verification: ver, Observation: ver.Observation}
		}
		d.Actions = append(d.Actions, v)
	}
	a.reply(w, r, d)
}

type approveRequest struct {
	// Digest is the one the approver was shown.
	Digest string `json:"digest"`
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if !a.decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	approver := actor(r)
	var act store.Action
	err := a.Store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		act, err = a.Store.Approve(ctx, tx, r.PathValue("id"), req.Digest, approver, a.now())
		if err != nil {
			return err
		}
		err = a.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: act.IncidentID, From: incident.AwaitingApproval, To: incident.Authorized,
			Reason: "Approved by " + approver + ".", Actor: approver,
		})
		if err != nil {
			return err
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultApproved).Inc)
		return a.Store.EnqueueTx(ctx, tx, governance.ExecuteArgs{ActionID: act.ID, IncidentID: act.IncidentID})
	})
	if err != nil {
		countRefusal(err)
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, viewAction(act))
}

type denyRequest struct {
	Reason string `json:"reason"`
}

func (a *API) deny(w http.ResponseWriter, r *http.Request) {
	var req denyRequest
	if !a.decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	approver := actor(r)
	err := a.Store.InTx(ctx, func(tx pgx.Tx) error {
		act, err := a.Store.Action(ctx, tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := a.Store.SetActionState(ctx, tx, act.ID, store.ActionAwaitingApproval, store.ActionDenied); err != nil {
			return store.ErrAlreadyDecided
		}
		reason := "Denied by " + approver + "."
		if req.Reason != "" {
			reason = "Denied by " + approver + ": " + req.Reason
		}
		for _, step := range [][2]incident.State{{incident.AwaitingApproval, incident.Recommended}, {incident.Recommended, incident.ClosedNoAction}} {
			if err := a.Store.Transition(ctx, tx, store.TransitionArgs{ID: act.IncidentID, From: step[0], To: step[1], Reason: reason, Actor: approver}); err != nil {
				return err
			}
		}
		store.AfterCommit(tx, approvalsTotal.WithLabelValues(resultDenied).Inc)
		return nil
	})
	if err != nil {
		countRefusal(err)
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, map[string]string{"state": string(store.ActionDenied)})
}

// suggest takes the same request as the gRPC API, in its JSON form.
func (a *API) suggest(w http.ResponseWriter, r *http.Request) {
	if a.Suggestions == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req themisv1.SubmitProposalRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.IncidentId = r.PathValue("id")
	person := ""
	if who, _ := people.From(r.Context()); !who.Token {
		person = who.Username
	}
	resp, err := a.Suggestions.SubmitFromPerson(r.Context(), person, &req)
	if err != nil {
		st := status.Convert(err)
		http.Error(w, st.Message(), suggestionStatuses[st.Code()])
		return
	}
	data, err := protojson.Marshal(resp)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// suggestionStatuses map what the core says about a suggestion to HTTP.
var suggestionStatuses = map[codes.Code]int{
	codes.InvalidArgument:    http.StatusBadRequest,
	codes.PermissionDenied:   http.StatusForbidden,
	codes.NotFound:           http.StatusNotFound,
	codes.ResourceExhausted:  http.StatusTooManyRequests,
	codes.FailedPrecondition: http.StatusConflict,
	codes.Internal:           http.StatusInternalServerError,
	codes.Unavailable:        http.StatusServiceUnavailable,
	codes.Canceled:           http.StatusServiceUnavailable,
	codes.DeadlineExceeded:   http.StatusServiceUnavailable,
}

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
		http.Error(w, "The time to approve this fix has run out.", http.StatusGone)
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

// changes diffs an action's templates, and says so when it cannot.
func changes(a store.Action) []rollback.Change {
	var pre, desired corev1.PodTemplateSpec
	if json.Unmarshal(a.PreState, &pre) != nil || json.Unmarshal(a.Desired, &desired) != nil {
		return []rollback.Change{{Field: "template", Before: "cannot be compared", After: "cannot be compared"}}
	}
	return rollback.Changes(pre, desired)
}
