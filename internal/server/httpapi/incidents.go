package httpapi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/hint"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
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
		v.Summary = viewSummary(summaries[in.ID], in.Target.Name, a.RootCauses.For(in.Profiles()...), a.Fixes)
		out = append(out, v)
	}
	a.reply(w, r, out)
}

func (a *API) detail(w http.ResponseWriter, r *http.Request) {
	rec, err := a.record(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Raw evidence, such as logs, is for responders: others learn that some
	// looks like instructions, and where, but not what it says.
	who, _ := identity.From(r.Context())
	a.reply(w, r, a.viewDetail(rec, who.Can(config.RoleResponder)))
}

// incidentRecord is what is stored about an incident that its page shows.
type incidentRecord struct {
	incident      store.Incident
	again         investigation.Offer
	transitions   []store.Transition
	evidence      []evidence.Item
	predicates    []check.Result
	investigation *store.InvestigationRecord
	gaps          []store.EvidenceGap
	proposals     []store.Proposal
	actions       []actionRecord
}

// actionRecord is an action, with its execution and verification once
// there are any.
type actionRecord struct {
	action       store.Action
	execution    *store.Execution
	verification *store.Verification
}

// record reads what is stored about the incident id.
func (a *API) record(ctx context.Context, id string) (incidentRecord, error) {
	var rec incidentRecord
	var err error
	if rec.incident, err = a.Store.Incident(ctx, id); err != nil {
		return rec, err
	}
	if a.Again != nil {
		if rec.again, err = a.Again.Offer(ctx, rec.incident); err != nil {
			return rec, err
		}
	}
	if rec.transitions, err = a.Store.Transitions(ctx, id); err != nil {
		return rec, err
	}
	if rec.evidence, err = a.Store.Evidence(ctx, id); err != nil {
		return rec, err
	}
	if rec.predicates, err = a.Store.Predicates(ctx, id); err != nil {
		return rec, err
	}
	if inv, err := a.Store.IncidentInvestigation(ctx, id); err == nil {
		rec.investigation = &inv
	} else if !errors.Is(err, store.ErrNotFound) {
		return rec, err
	}
	if rec.gaps, err = a.Store.EvidenceGaps(ctx, id); err != nil {
		return rec, err
	}
	if rec.proposals, err = a.Store.Proposals(ctx, a.Store.DB(), id); err != nil {
		return rec, err
	}
	actions, err := a.Store.IncidentActions(ctx, a.Store.DB(), id)
	if err != nil {
		return rec, err
	}
	for _, act := range actions {
		ar := actionRecord{action: act}
		if e, err := a.Store.ActionExecution(ctx, a.Store.DB(), act.ID); err == nil {
			ar.execution = &e
		}
		if v, err := a.Store.Verification(ctx, a.Store.DB(), act.ID); err == nil {
			ar.verification = &v
		}
		rec.actions = append(rec.actions, ar)
	}
	return rec, nil
}

// viewDetail is the incident's page, with raw evidence when raw is set.
func (a *API) viewDetail(rec incidentRecord, raw bool) detailView {
	in := rec.incident
	d := detailView{
		Incident: viewIncident(in), Transitions: rec.transitions, Predicates: rec.predicates,
		Checks: map[string]checkWords{}, Causes: map[string]string{}, AnotherApprover: a.anotherApprover(),
	}
	if rec.again.Offered {
		d.InvestigateAgain = &againView{Refused: rec.again.Refused}
	}
	for _, p := range rec.proposals {
		d.Proposals = append(d.Proposals, proposalView{Proposal: p, Words: a.Fixes.Words(p.Capability, p.Target.Name, p.Params, nil, nil)})
	}
	for _, c := range a.CheckList {
		d.Checks[c.Name] = checkWords{True: c.Description, False: c.WhenFalse, Topic: c.Topic, Reads: c.Reads, Availability: c.Availability}
	}
	d.Basis = a.basis(rec)
	for _, c := range a.RootCauses.For(in.Profiles()...) {
		d.Causes[c.Name] = c.Description
	}
	numbers := map[string]citeView{}
	for i, it := range rec.evidence {
		var fix evidence.ThemisFixView
		if it.Template == evidence.ThemisFix && d.Follows == nil && json.Unmarshal(it.Data, &fix) == nil {
			d.Follows = &followsView{Incident: fix.Incident, Done: fix.Done, AppliedAt: fix.AppliedAt, Before: int64(in.CreatedAt.Sub(fix.AppliedAt).Seconds())}
		}
		v := evidenceView{Item: it, Number: i + 1, Kind: kindOf(it.Template), Later: it.CollectedAt.Sub(rec.evidence[0].CollectedAt) > laterBy}
		numbers[it.ID] = citeView{N: v.Number, Kind: v.Kind}
		if raw {
			v.Data = it.Data
			v.Digest, v.View = shown(it, rec.evidence)
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
	d.EvidenceWarning = evidence.Warning(rec.evidence, raw)
	d.Facts = a.facts(in, rec.predicates, func(ids []string) []citeView {
		var out []citeView
		for _, id := range ids {
			if c, ok := numbers[id]; ok && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
		slices.SortFunc(out, func(a, b citeView) int { return a.N - b.N })
		return out
	})
	d.Situation, d.Symptoms = a.situation(in, rec.predicates), a.symptoms(in)
	d.Collectable, d.Previewed = a.collectable(in, rec.evidence), a.Previews != nil
	if inv := rec.investigation; inv != nil {
		d.Investigation = inv
		// Once someone's suggestion moves it on, what's next matters more,
		// and an incident closed for another reason says that reason.
		if inv.Conclusion == "inconclusive" && (in.State == incident.Inconclusive || in.State == incident.ClosedNoAction && closedBy(rec.transitions, investigation.Investigator)) {
			d.Headline = hint.Headline(inv.Blind)
		}
	}
	d.Missing = a.Remedy.Messages(in, rec.gaps)
	d.Fixes = a.offered(in)
	for _, ar := range rec.actions {
		v := viewAction(ar.action, a.Fixes)
		v.Execution = ar.execution
		if ver := ar.verification; ver != nil {
			v.Verification = &verificationView{Verification: *ver, Observation: ver.Observation}
		}
		d.Actions = append(d.Actions, v)
	}
	if a.Renewals != nil && len(rec.actions) > 0 {
		actions := make([]store.Action, 0, len(rec.actions))
		for _, ar := range rec.actions {
			actions = append(actions, ar.action)
		}
		d.Actions[len(d.Actions)-1].Renewable = a.Renewals.Renewable(in, actions)
	}
	return d
}

// laterBy is how long after the first evidence an item counts as collected
// later, so the page says when.
const laterBy = time.Minute

// basis says, by name, what each check that only says whether figures are
// there, and found them, rests on: the history its evidence holds, such as
// "Prometheus's container CPU history over 1h".
func (a *API) basis(rec incidentRecord) map[string]string {
	out := map[string]string{}
	for _, p := range rec.predicates {
		if c, ok := a.CheckList.Get(p.Name); !ok || !c.Availability || p.Value != check.True {
			continue
		}
		for _, it := range rec.evidence {
			if h := it.History(); h != "" && slices.Contains(p.EvidenceIDs, it.ID) {
				out[p.Name] = h
			}
		}
	}
	return out
}

// offered are the fixes the incident's profiles still configured offer, as
// its investigator and agents get them. Without the configuration, none.
func (a *API) offered(in store.Incident) []offeredFix {
	if a.Config == nil {
		return nil
	}
	var out []offeredFix
	for _, name := range a.Config.Current().Matched(in.Profiles()).Capabilities() {
		if f, ok := a.Fixes[name]; ok {
			out = append(out, offeredFix{Name: f.Name(), Noun: f.Noun(), Description: f.Description(), Schema: jsontext.Value(f.Schema())})
		}
	}
	return out
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

// closedBy reports whether actor made the incident's last change of state.
func closedBy(transitions []store.Transition, actor string) bool {
	return len(transitions) > 0 && transitions[len(transitions)-1].Actor == actor
}
