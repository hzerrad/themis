package httpapi

import (
	"net/http"

	"github.com/hzerrad/themis/internal/incident/governance"
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
	act, err := governance.Approve(r.Context(), a.Store, r.PathValue("id"), req.Digest, actor(r), a.now())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, viewAction(act, a.Fixes))
}

type denyRequest struct {
	Reason string `json:"reason"`
}

func (a *API) deny(w http.ResponseWriter, r *http.Request) {
	var req denyRequest
	if !a.decode(w, r, &req) {
		return
	}
	if err := governance.Deny(r.Context(), a.Store, r.PathValue("id"), actor(r), req.Reason); err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, map[string]string{"state": string(store.ActionDenied)})
}
