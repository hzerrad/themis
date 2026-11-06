package alertmanager

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hzerrad/themis/internal/config"
)

// SetupStep is one step of having Alertmanager send alerts to Themis. Code,
// when there is some, is to copy and run.
type SetupStep struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Code  string `json:"code,omitempty"`
}

// tokenSecret is the Secret the steps copy the token into, unless the
// install manages one.
const tokenSecret = "themis-intake-token"

// SetupSteps say how to have an Alertmanager in alertmanagerNS send the
// alerts named to Themis in themisNS: through the Prometheus Operator, or
// any Alertmanager. The token is copied from Secret to Secret, so it's never
// shown, or read from the Secret the install manages.
func SetupSteps(in config.Intake, themisNS, alertmanagerNS string, names []string) []SetupStep {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	matcher := fmt.Sprintf(`'alertname=~"%s"'`, strings.Join(quoted, "|"))
	secret := tokenSecret
	if in.AlertmanagerSecret != "" {
		secret = in.AlertmanagerSecret
	}

	operator := SetupStep{
		Title: "With the Prometheus Operator",
		Text: "Set this in the chart's values and upgrade. The chart makes an AlertmanagerConfig in each enrolled namespace, with a Secret holding the intake token. " +
			"The operator routes only that namespace's alerts through it. Anyone who can read Secrets there can then send alerts to Themis.",
		Code: "alerting:\n  webhook:\n    alertmanagerConfig:\n      enabled: true",
	}
	switch {
	case in.AlertmanagerConfigs:
		operator.Text, operator.Code = "Done. The chart made an AlertmanagerConfig in each enrolled namespace.", ""
	case in.AlertmanagerSecret != "":
		operator.Text = fmt.Sprintf("Set this in the chart's values and upgrade. The chart makes an AlertmanagerConfig in each enrolled namespace, "+
			"which reads the token from the %s Secret you manage there. The operator routes only that namespace's alerts through it.", secret)
	}

	token := SetupStep{
		Title: "With any other Alertmanager: the intake token",
		Text:  "This copies the token Alertmanager must send from Themis's Secret into a Secret in Alertmanager's namespace, without showing it. Under GitOps, manage that Secret with your secrets tool instead, as for tokens.existingSecret, and set alerting.webhook.tokenSecret to its name.",
		Code: fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.data.intake}' | base64 -d | \\\n  kubectl -n %s create secret generic %s --from-file=token=/dev/stdin",
			themisNS, in.TokenSecret, alertmanagerNS, secret),
	}
	if in.AlertmanagerSecret != "" {
		token.Text, token.Code = fmt.Sprintf("Alertmanager reads the token from the %s Secret you manage in %s, its token key.", secret, alertmanagerNS), ""
	}

	receiver := SetupStep{
		Title: "With any other Alertmanager: the route",
		Text: fmt.Sprintf("Mount the %s Secret into Alertmanager at /etc/alertmanager/themis, and add this to alertmanager.yml. "+
			"Put this route first. With continue: true, the alerts go on to the routes after it.", secret),
		Code: fmt.Sprintf(`receivers:
  - name: themis
    webhook_configs:
      - url: %s
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/themis/token
route:
  routes:
    - receiver: themis
      matchers:
        - %s
      group_by: [alertname, namespace, pod]
      continue: true`, in.URL, matcher),
	}
	return []SetupStep{operator, token, receiver}
}
