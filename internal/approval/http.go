// Package approval serves the HTTP API behind the UI: incidents, and the
// approval or denial of the actions awaiting a human.
package approval

import (
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/telemetry"
)

// approver is the identity of the bootstrap admin token.
const approver = "admin"

const maxBodyBytes = 64 << 10

type API struct {
	Store     *store.Store
	TokenFile string
	Logger    *slog.Logger
	Now       func() time.Time
}

func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("GET /v1/incidents", a.authed(a.list, nil))
	mux.Handle("GET /v1/incidents/{id}", a.authed(a.detail, incidentFields))
	mux.Handle("POST /v1/actions/{id}/approve", a.authed(a.approve, a.actionFields))
	mux.Handle("POST /v1/actions/{id}/deny", a.authed(a.deny, a.actionFields))
}

// authed serves h to the admin, with the log fields of the request's path,
// and logs the request at debug level.
func (a *API) authed(h http.HandlerFunc, fields func(*http.Request) []slog.Attr) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &statusWriter{ResponseWriter: rw, status: http.StatusOK}
		defer func() {
			a.logger().DebugContext(r.Context(), "API request served", "route", r.Pattern, "status", w.status)
		}()
		want, err := auth.ReadToken(a.TokenFile)
		if err != nil {
			a.logger().ErrorContext(r.Context(), "cannot read the admin token", "file", a.TokenFile, telemetry.FieldError, err)
			http.Error(w, "API temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !auth.BearerMatches(r.Header.Get("Authorization"), want) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if fields != nil {
			r = r.WithContext(telemetry.WithAttrs(r.Context(), fields(r)...))
		}
		h(w, r)
	})
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
	for _, it := range items {
		d.Evidence = append(d.Evidence, evidenceView{Item: it, Data: it.Data})
	}
	if d.Predicates, err = a.Store.Predicates(ctx, in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if inv, err := a.Store.IncidentInvestigation(ctx, in.ID); err == nil {
		d.Investigation = &inv
	} else if !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
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
	var act store.Action
	err := a.Store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		act, err = a.Store.Approve(ctx, tx, r.PathValue("id"), req.Digest, approver, a.now())
		if err != nil {
			return err
		}
		err = a.Store.Transition(ctx, tx, store.TransitionArgs{
			ID: act.IncidentID, From: incident.AwaitingApproval, To: incident.Authorized,
			Reason: "approved digest " + req.Digest, Actor: approver,
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
	err := a.Store.InTx(ctx, func(tx pgx.Tx) error {
		act, err := a.Store.Action(ctx, tx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if err := a.Store.SetActionState(ctx, tx, act.ID, store.ActionAwaitingApproval, store.ActionDenied); err != nil {
			return store.ErrAlreadyDecided
		}
		reason := "denied by " + approver
		if req.Reason != "" {
			reason += ": " + req.Reason
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

func (a *API) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxBodyBytes), v); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
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
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrDigestMismatch):
		http.Error(w, "the action changed since it was shown: reload and review it", http.StatusConflict)
	case errors.Is(err, store.ErrAlreadyDecided), errors.Is(err, store.ErrStateConflict):
		http.Error(w, "the action is no longer awaiting approval", http.StatusConflict)
	case errors.Is(err, store.ErrExpired):
		http.Error(w, "the approval window has passed", http.StatusGone)
	default:
		a.logger().ErrorContext(r.Context(), "API request failed", telemetry.FieldError, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
