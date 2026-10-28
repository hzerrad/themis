// Package approval serves the HTTP API behind the UI: incidents, and the
// approval or denial of the actions awaiting a human.
package approval

import (
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hzerrad/themis/internal/auth"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"
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
	mux.Handle("GET /v1/incidents", a.authed(a.list))
	mux.Handle("GET /v1/incidents/{id}", a.authed(a.detail))
	mux.Handle("POST /v1/actions/{id}/approve", a.authed(a.approve))
	mux.Handle("POST /v1/actions/{id}/deny", a.authed(a.deny))
}

func (a *API) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, err := auth.ReadToken(a.TokenFile)
		if err != nil {
			a.logger().Error("cannot read the admin token", "file", a.TokenFile, "error", err)
			http.Error(w, "API temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !auth.BearerMatches(r.Header.Get("Authorization"), want) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	incidents, err := a.Store.ListIncidents(r.Context(), 100)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := make([]incidentView, 0, len(incidents))
	for _, in := range incidents {
		out = append(out, viewIncident(in))
	}
	a.reply(w, out)
}

func (a *API) detail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, err := a.Store.Incident(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	d := detailView{Incident: viewIncident(in)}
	if d.Transitions, err = a.Store.Transitions(ctx, in.ID); err != nil {
		a.fail(w, err)
		return
	}
	items, err := a.Store.Evidence(ctx, in.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	for _, it := range items {
		d.Evidence = append(d.Evidence, evidenceView{Item: it, Data: it.Data})
	}
	if d.Predicates, err = a.Store.Predicates(ctx, in.ID); err != nil {
		a.fail(w, err)
		return
	}
	if inv, err := a.Store.IncidentInvestigation(ctx, in.ID); err == nil {
		d.Investigation = &inv
	} else if !errors.Is(err, store.ErrNotFound) {
		a.fail(w, err)
		return
	}
	if d.Proposals, err = a.Store.Proposals(ctx, a.Store.DB(), in.ID); err != nil {
		a.fail(w, err)
		return
	}
	actions, err := a.Store.IncidentActions(ctx, a.Store.DB(), in.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	for _, act := range actions {
		v := actionView{Action: act}
		if e, err := a.Store.ActionExecution(ctx, a.Store.DB(), act.ID); err == nil {
			v.Execution = &e
		}
		d.Actions = append(d.Actions, v)
	}
	a.reply(w, d)
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
		return a.Store.EnqueueTx(ctx, tx, governance.ExecuteArgs{ActionID: act.ID})
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.reply(w, act)
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
		return nil
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.reply(w, map[string]string{"state": string(store.ActionDenied)})
}

func (a *API) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxBodyBytes), v); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (a *API) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, v); err != nil {
		a.logger().Error("cannot write a response", "error", err)
	}
}

func (a *API) fail(w http.ResponseWriter, err error) {
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
		a.logger().Error("API request failed", "error", err)
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
