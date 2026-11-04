package alertmanager

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hzerrad/themis/internal/config"
)

// SetupStep is one step of connecting Alertmanager to Themis, to copy and run.
type SetupStep struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Code  string `json:"code"`
}

// SetupSteps say how to send the alerts named to Themis, from an Alertmanager
// in alertmanagerNS, for Themis in themisNS. The token is copied from Secret to
// Secret, so it's never shown.
func SetupSteps(in config.Intake, themisNS, alertmanagerNS string, names []string) []SetupStep {
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
