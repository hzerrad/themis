package httpapi

import (
	"errors"
	"net/http"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	incidents, err := a.Store.ListIncidents(r.Context(), 100)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	ids := make([]string, 0, len(incidents))
	for _, in := range incidents {
		ids = append(ids, in.ID)
	}
	summaries, err := a.Store.IncidentSummaries(r.Context(), ids)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]incidentView, 0, len(incidents))
	for _, in := range incidents {
		v := viewIncident(in)
		v.Summary = viewSummary(summaries[in.ID], a.RootCauses.For(in.Profiles()...))
		out = append(out, v)
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
	d := detailView{Incident: viewIncident(in), Checks: map[string]checkWords{}, Causes: map[string]string{}}
	for _, c := range a.CheckList {
		d.Checks[c.Name] = checkWords{True: c.Description, False: c.WhenFalse, Topic: c.Topic, Reads: c.Reads}
	}
	for _, c := range a.RootCauses.For(in.Profiles()...) {
		d.Causes[c.Name] = c.Description
	}
	if d.Transitions, err = a.Store.Transitions(ctx, in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := a.Store.Evidence(ctx, in.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Raw evidence, such as logs, is for responders: others learn that some
	// looks like instructions, and where, but not what it says.
	who, _ := identity.From(ctx)
	raw := who.Can(config.RoleResponder)
	for _, it := range items {
		v := evidenceView{Item: it}
		if raw {
			v.Data = it.Data
		}
		for _, s := range it.Suspicious {
			sv := suspicionView{Reason: s.Reason, At: s.At, Origin: it.Origin(s), What: s.What()}
			if raw {
				sv.Excerpt = s.Excerpt
			}
			v.Suspicious = append(v.Suspicious, sv)
		}
		d.Evidence = append(d.Evidence, v)
	}
	d.EvidenceWarning = evidence.Warning(items, raw)
	if d.Predicates, err = a.Store.Predicates(ctx, in.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if inv, err := a.Store.IncidentInvestigation(ctx, in.ID); err == nil {
		d.Investigation = &inv
		// Once someone's suggestion moves it on, what's next matters more.
		if inv.Conclusion == "inconclusive" && (in.State == incident.Inconclusive || in.State == incident.ClosedNoAction) {
			d.Headline = hint.Headline(inv.Blind)
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
		v := viewAction(act, a.Fixes)
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

// changes lists what an action changes, as its fix describes it, and says so
// when it cannot.
func changes(a store.Action, fixes capability.Registry) []capability.Change {
	unknown := []capability.Change{{Field: "template", Before: "cannot be compared", After: "cannot be compared"}}
	f, ok := fixes[a.Capability]
	if !ok {
		return unknown
	}
	list, err := f.Changes(a.PreState, a.Desired)
	if err != nil {
		return unknown
	}
	return list
}
