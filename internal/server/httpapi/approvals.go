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

// Reinvestigation has an incident waiting for a suggested fix investigated
// again.
type Reinvestigation interface {
	Offer(ctx context.Context, in store.Incident) (investigation.Offer, error)
	Start(ctx context.Context, incidentID, by string) error
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
