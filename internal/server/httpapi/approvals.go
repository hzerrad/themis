package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"
)

type approveRequest struct {
	// Digest is the one the approver was shown.
	Digest string `json:"digest"`
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if !a.decode(w, r, &req) {
		return
	}
	// A fix put to a person before the install stopped the executor applying
	// it, such as by turning the executor off, isn't approved: nothing would
	// apply it.
	if pending, err := a.Store.Action(r.Context(), a.Store.DB(), r.PathValue("id")); err == nil {
		if f, ok := a.Fixes[pending.Capability]; ok {
			if why := a.Reach.Why(pending.Target.Namespace, f); why != "" {
				http.Error(w, "Refused. "+why, http.StatusConflict)
				return
			}
		}
	}
	act, err := governance.Approve(r.Context(), a.Store, r.PathValue("id"), req.Digest, actor(r), a.now(), a.anotherApprover())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, viewAction(act, a.Fixes))
}

// anotherApprover reports whether a fix needs an approver other than the
// person who suggested it.
func (a *API) anotherApprover() bool {
	return a.Config != nil && a.Config.Current().Approvals.RequireAnotherApprover
}

type denyRequest struct {
	Reason string `json:"reason"`
	// Close closes the incident with no change. Otherwise it waits for
	// another fix.
	Close bool `json:"close"`
}

func (a *API) deny(w http.ResponseWriter, r *http.Request) {
	var req denyRequest
	if !a.decode(w, r, &req) {
		return
	}
	deny := governance.Deny
	if req.Close {
		deny = governance.DenyAndClose
	}
	if err := deny(r.Context(), a.Store, r.PathValue("id"), actor(r), req.Reason, a.now()); err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, map[string]string{"state": string(store.ActionDenied)})
}

// Renewals ask again for approval of a fix whose approval expired with no
// decision, and say whether a person can, from what Themis keeps.
type Renewals interface {
	Renewable(in store.Incident, actions []store.Action) bool
	Renew(ctx context.Context, actionID, by string) (store.Action, error)
}

// renew asks again for approval of a fix whose approval expired, and answers
// with the fix that waits for approval now.
func (a *API) renew(w http.ResponseWriter, r *http.Request) {
	act, err := a.Renewals.Renew(r.Context(), r.PathValue("id"), actor(r))
	if refused, ok := errors.AsType[*governance.Refusal](err); ok {
		http.Error(w, refused.Reason, http.StatusConflict)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, viewAction(act, a.Fixes))
}

// Reinvestigation has an incident waiting for a suggested fix investigated
// again.
type Reinvestigation interface {
	Offer(ctx context.Context, in store.Incident) (investigation.Offer, error)
	Start(ctx context.Context, incidentID, by string) error
}

type closeRequest struct {
	Reason string `json:"reason"`
}

// close closes an incident waiting for a suggested fix, with no change.
func (a *API) close(w http.ResponseWriter, r *http.Request) {
	var req closeRequest
	if !a.decode(w, r, &req) {
		return
	}
	err := investigation.Close(r.Context(), a.Store, r.PathValue("id"), actor(r), req.Reason)
	var refused *investigation.Refusal
	if errors.As(err, &refused) {
		http.Error(w, "Refused. "+refused.Reason, http.StatusConflict)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, map[string]string{"state": "closed_no_action"})
}

// investigate has the incident investigated again.
func (a *API) investigate(w http.ResponseWriter, r *http.Request) {
	err := a.Again.Start(r.Context(), r.PathValue("id"), actor(r))
	var refused *investigation.Refusal
	if errors.As(err, &refused) {
		http.Error(w, "Refused. "+refused.Reason, http.StatusConflict)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, map[string]string{"state": "investigating"})
}
