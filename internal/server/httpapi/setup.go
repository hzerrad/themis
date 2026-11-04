package httpapi

import (
	"net/http"
	"regexp"
	"slices"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
)

var namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// alertmanagerSetup says how to send Alertmanager's alerts to Themis, for an
// Alertmanager in the namespace the query names. The token is copied from
// Secret to Secret, so it's never shown.
func (a *API) alertmanagerSetup(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = "monitoring"
	}
	if !namespacePattern.MatchString(ns) {
		http.Error(w, "That isn't a namespace name.", http.StatusBadRequest)
		return
	}
	in := a.Intake
	if in.URL == "" || in.TokenSecret == "" || a.Namespace == "" {
		http.Error(w, "Themis doesn't know its own address. Install it with the chart, or see the configuration reference for connecting Alertmanager.", http.StatusNotFound)
		return
	}
	a.reply(w, r, map[string][]alertmanager.SetupStep{"steps": alertmanager.SetupSteps(in, a.Namespace, ns, alertNames(a.Config.Current()))})
}

// alertNames are the alerts some profile handles, so Alertmanager sends only
// those.
func alertNames(cfg config.Config) []string {
	var names []string
	for _, p := range cfg.Profiles {
		names = append(names, p.Match.AlertNames...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
