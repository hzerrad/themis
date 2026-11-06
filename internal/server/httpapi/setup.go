package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/incident/intake/rules"
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
	a.reply(w, r, map[string][]alertmanager.SetupStep{"steps": alertmanager.SetupSteps(in, a.Namespace, ns, a.Config.Current().AlertNames())})
}

// AlertmanagerConnection is the Alertmanager Themis reads, whose address the
// Setup page may set unless the install did.
type AlertmanagerConnection interface {
	View(ctx context.Context) (settings.AlertmanagerView, error)
	SetAddress(ctx context.Context, address, by string) error
	Test(ctx context.Context, address string, answered []string) (alertmanager.TestResult, error)
}

func (a *API) alertmanagerSettings(w http.ResponseWriter, r *http.Request) {
	v, err := a.Alertmanager.View(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.reply(w, r, v)
}

type alertmanagerChange struct {
	Address string `json:"address"`
}

// setAlertmanager sets the address Themis reads Alertmanager at, from the
// next read. An empty one goes back to the address found at install.
func (a *API) setAlertmanager(w http.ResponseWriter, r *http.Request) {
	var change alertmanagerChange
	if !a.decode(w, r, &change) {
		return
	}
	err := a.Alertmanager.SetAddress(r.Context(), change.Address, actor(r))
	switch {
	case errors.Is(err, settings.ErrSetByInstall):
		http.Error(w, "That was set when Themis was installed. Change it in the Helm values instead.", http.StatusConflict)
		return
	case errors.Is(err, settings.ErrReadingOff) || errors.Is(err, settings.ErrNotHTTPURL):
		http.Error(w, sentence(err), http.StatusBadRequest)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	a.logger().InfoContext(r.Context(), "the Alertmanager address was changed", "by", actor(r))
	a.alertmanagerSettings(w, r)
}

// AlertRules says which alerts have alerting rules in Prometheus.
type AlertRules interface {
	Check(ctx context.Context, answered []string) (rules.Result, error)
}

type alertRulesView struct {
	// Answered are the alerts the profiles answer.
	Answered []string `json:"answered"`
	// Checked says Prometheus was asked which exist. Error says why it
	// couldn't be.
	Checked bool     `json:"checked"`
	Address string   `json:"address,omitempty"`
	Exist   []string `json:"exist"`
	Missing []string `json:"missing"`
	Error   string   `json:"error,omitempty"`
	// RuleFile and PrometheusRule are the shipped rules for the alerts
	// missing, or for every alert answered when unchecked. Standard says one
	// of them is a rule kube-prometheus-stack has.
	RuleFile       string `json:"ruleFile,omitempty"`
	PrometheusRule string `json:"prometheusRule,omitempty"`
	Standard       bool   `json:"standard"`
	// Unshipped are the alerts among them Themis ships no rule for.
	Unshipped []string `json:"unshipped"`
}

// alertRules says which alerts the profiles answer have alerting rules in
// Prometheus, and gives the rules Themis ships for those missing.
func (a *API) alertRules(w http.ResponseWriter, r *http.Request) {
	answered := a.Config.Current().AlertNames()
	shipped, err := rules.Shipped()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := alertRulesView{Answered: answered, Exist: []string{}, Missing: []string{}}
	show := answered
	if a.Rules != nil {
		res, err := a.Rules.Check(r.Context(), answered)
		if err != nil {
			v.Error = sentence(err)
		} else {
			v.Checked, v.Address, v.Exist, v.Missing, show = true, res.Address, res.Exist, res.Missing, res.Missing
		}
	}
	file, standard, none := shipped.For(show)
	v.Standard, v.Unshipped = standard, append([]string{}, none...)
	if len(file.Groups) > 0 {
		v.RuleFile, v.PrometheusRule = file.YAML(), file.PrometheusRule("themis-alert-rules")
	}
	a.reply(w, r, v)
}

// testAlertmanager reads Alertmanager at the address given, or at the one in
// use, and says which of the alerts Themis answers fire now.
func (a *API) testAlertmanager(w http.ResponseWriter, r *http.Request) {
	var change alertmanagerChange
	if !a.decode(w, r, &change) {
		return
	}
	got, err := a.Alertmanager.Test(r.Context(), strings.TrimSpace(change.Address), a.Config.Current().AlertNames())
	if err != nil {
		http.Error(w, sentence(err), http.StatusBadGateway)
		return
	}
	a.reply(w, r, got)
}
