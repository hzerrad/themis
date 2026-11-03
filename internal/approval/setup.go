package approval

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/hzerrad/themis/internal/config"
)

// SetupStep is one step of connecting something to Themis, to copy and run.
type SetupStep struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Code  string `json:"code"`
}

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
	a.reply(w, r, map[string][]SetupStep{"steps": alertmanagerSteps(in, a.Namespace, ns, alertNames(a.Config.Current()))})
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

func alertmanagerSteps(in config.Intake, themisNS, alertmanagerNS string, names []string) []SetupStep {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	matcher := fmt.Sprintf(`'alertname=~"%s"'`, strings.Join(quoted, "|"))
	receiver := func(tokenFile string) string {
		return fmt.Sprintf(`receivers:
  - name: themis
    webhook_configs:
      - url: %s
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials_file: %s
route:
  routes:
    - receiver: themis
      matchers:
        - %s
      group_by: [alertname, namespace, pod]
      continue: true`, in.URL, tokenFile, matcher)
	}
	indent := func(s, by string) string { return by + strings.ReplaceAll(s, "\n", "\n"+by) }
	return []SetupStep{
		{
			Title: "Copy the intake token to Alertmanager",
			Text:  "This copies the token Alertmanager must send from Themis's Secret into a Secret in Alertmanager's namespace, without showing it.",
			Code: fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.data.intake}' | base64 -d | \\\n  kubectl -n %s create secret generic themis-intake-token --from-file=token=/dev/stdin",
				themisNS, in.TokenSecret, alertmanagerNS),
		},
		{
			Title: "With kube-prometheus-stack",
			Text:  "Add this to the chart's values and upgrade. These lists replace the chart's own, so keep your existing receivers and routes next to Themis's.",
			Code:  "alertmanager:\n  alertmanagerSpec:\n    secrets: [themis-intake-token]\n  config:\n" + indent(receiver("/etc/alertmanager/secrets/themis-intake-token/token"), "    "),
		},
		{
			Title: "With any other Alertmanager",
			Text:  "Mount the themis-intake-token Secret into Alertmanager, add this to alertmanager.yml, and point credentials_file at the mounted token.",
			Code:  receiver("/etc/alertmanager/themis/token"),
		},
	}
}
