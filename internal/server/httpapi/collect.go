package httpapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"
)

// Collector collects the evidence a person asks for, through the templates
// the incident's profiles allow, and decides its checks again.
type Collector interface {
	Collect(ctx context.Context, incidentID, person, template string, params map[string]string) (evidence.Item, int, error)
}

type collectRequest struct {
	Template string            `json:"template"`
	Params   map[string]string `json:"params,omitempty"`
}

// collectedView is the piece of evidence collected, by its number in the
// incident's evidence.
type collectedView struct {
	Number int    `json:"number"`
	Kind   string `json:"kind"`
}

// askStatuses say in HTTP why a request for evidence was refused.
var askStatuses = map[investigation.AskKind]int{
	investigation.AskInvalid:    http.StatusBadRequest,
	investigation.AskNotAllowed: http.StatusForbidden,
	investigation.AskBusy:       http.StatusConflict,
	investigation.AskClosed:     http.StatusConflict,
	investigation.AskTooMany:    http.StatusTooManyRequests,
	investigation.AskFailed:     http.StatusBadGateway,
}

// collect collects a piece of evidence a person asks for.
func (a *API) collect(w http.ResponseWriter, r *http.Request) {
	var req collectRequest
	if !a.decode(w, r, &req) {
		return
	}
	who, _ := identity.From(r.Context())
	item, n, err := a.Collect.Collect(r.Context(), r.PathValue("id"), who.Username, req.Template, req.Params)
	var refused *investigation.AskError
	switch {
	case errors.As(err, &refused):
		http.Error(w, refused.Message, askStatuses[refused.Kind])
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, collectedView{Number: n, Kind: kindOf(item.Template)})
}

// collectView is a template a person may ask to read for an incident, and
// what each of its parameters may be.
type collectView struct {
	Template string      `json:"template"`
	Kind     string      `json:"kind"`
	Params   []paramView `json:"params,omitempty"`
}

type paramView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Choices are the values it may take, from the incident's evidence;
	// none means any.
	Choices []choiceView `json:"choices,omitempty"`
}

type choiceView struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// windows are the windows a person may read figures over, beyond the hour
// Themis reads first.
var windows = []choiceView{{"6h", "6 hours"}, {"24h", "24 hours"}, {"168h", "7 days"}}

// collectable are the templates a person may ask to read for in, those its
// profiles allow, with the pods and containers its evidence names for their
// parameters. None while the incident takes no evidence, or without a
// collector.
func (a *API) collectable(in store.Incident, items []evidence.Item) []collectView {
	if a.Collect == nil || !investigation.TakesEvidence(in.State) {
		return nil
	}
	ev := a.matched(in).Evidence()
	var templates []string
	for _, t := range slices.Concat(ev.Allowed, ev.Initial) {
		if !slices.Contains(templates, t) {
			templates = append(templates, t)
		}
	}
	var pods, containers []choiceView
	for i := len(items) - 1; i >= 0; i-- {
		var v evidence.PodStatesView
		if items[i].Template != evidence.PodStates || json.Unmarshal(items[i].Data, &v) != nil {
			continue
		}
		for _, p := range v.Pods {
			pods = append(pods, choiceView{p.Name, p.Name})
			for _, c := range p.Containers {
				if !slices.ContainsFunc(containers, func(o choiceView) bool { return o.Value == c.Name }) {
					containers = append(containers, choiceView{c.Name, c.Name})
				}
			}
		}
		break
	}
	var out []collectView
	for _, t := range templates {
		c := collectView{Template: t, Kind: kindOf(t)}
		switch t {
		case evidence.ContainerLogsTail:
			c.Params = []paramView{
				{Name: "pod", Label: "Pod", Choices: pods},
				{Name: "container", Label: "Container", Choices: containers},
				{Name: "previous", Label: "Run", Choices: []choiceView{{"true", "Previous run"}, {"false", "Current run"}}},
			}
		case evidence.ContainerMemory, evidence.ContainerCPU:
			c.Params = []paramView{{Name: evidence.WindowParam, Label: "Over", Choices: windows}}
		}
		out = append(out, c)
	}
	return out
}
