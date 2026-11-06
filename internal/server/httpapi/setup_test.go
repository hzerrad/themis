package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/server/httpapi"
)

func setupAPI(t *testing.T, intake config.Intake) http.Handler {
	t.Helper()
	e := newEnv(t)
	mux := http.NewServeMux()
	cfg := config.Config{Profiles: []config.Profile{
		{Match: config.Match{AlertNames: []string{"KubePodCrashLooping", "KubePodOOMKilled"}}},
		{Match: config.Match{AlertNames: []string{"KubePodCrashLooping", "Custom.Alert"}}},
	}}
	(&httpapi.API{Store: e.store, TokenFile: tokenFile(t), Config: cfg, Intake: intake, Namespace: "themis"}).Register(mux)
	return mux
}

func tokenFile(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func setupSteps(t *testing.T, in config.Intake) []alertmanager.SetupStep {
	t.Helper()
	in.URL, in.TokenSecret = "http://themis-core.themis.svc:8080/v1/intake/alertmanager", "themis-tokens"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/setup/alertmanager?namespace=observability", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	setupAPI(t, in).ServeHTTP(rec, req)
	var got struct{ Steps []alertmanager.SetupStep }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Steps) != 3 {
		t.Fatalf("setup = %d %q, %v", rec.Code, rec.Body, err)
	}
	return got.Steps
}

// The steps offer the chart's AlertmanagerConfigs, then any Alertmanager:
// the token copied without being shown, and the route to put first.
func TestAlertmanagerSetup(t *testing.T) {
	steps := setupSteps(t, config.Intake{})
	var values struct {
		Alerting struct {
			Webhook struct {
				AlertmanagerConfig struct{ Enabled bool } `json:"alertmanagerConfig"`
			}
		}
	}
	if err := yaml.Unmarshal([]byte(steps[0].Code), &values); err != nil || !values.Alerting.Webhook.AlertmanagerConfig.Enabled {
		t.Errorf("the operator's values = %+v, %v\n%s", values, err, steps[0].Code)
	}
	for _, want := range []string{"kubectl -n themis get secret themis-tokens", "kubectl -n observability create secret generic themis-intake-token"} {
		if !strings.Contains(steps[1].Code, want) {
			t.Errorf("token step %q lacks %q", steps[1].Code, want)
		}
	}
	if !strings.Contains(steps[2].Text, "Put this route first.") {
		t.Errorf("route step says %q", steps[2].Text)
	}
	var plain alertmanagerConfig
	if err := yaml.Unmarshal([]byte(steps[2].Code), &plain); err != nil {
		t.Fatalf("alertmanager.yml doesn't parse: %v\n%s", err, steps[2].Code)
	}
	plain.check(t, "/etc/alertmanager/themis/token")
}

// Under GitOps the steps read the token from the Secret the install manages,
// and say when the chart made the AlertmanagerConfigs.
func TestAlertmanagerSetup_GitOps(t *testing.T) {
	steps := setupSteps(t, config.Intake{AlertmanagerSecret: "intake-from-vault"}) //nolint:gosec // a Secret's name
	if steps[1].Code != "" || !strings.Contains(steps[1].Text, "intake-from-vault Secret you manage in observability") ||
		!strings.Contains(steps[0].Text, "intake-from-vault") || !strings.Contains(steps[2].Text, "Mount the intake-from-vault Secret") {
		t.Errorf("steps with a Secret the install manages = %+v", steps)
	}
	if steps := setupSteps(t, config.Intake{AlertmanagerConfigs: true}); steps[0].Code != "" || !strings.HasPrefix(steps[0].Text, "Done.") {
		t.Errorf("the operator's step once the chart made the AlertmanagerConfigs = %+v", steps[0])
	}
}

type alertmanagerConfig struct {
	Receivers []struct {
		Name           string
		WebhookConfigs []struct {
			URL          string `json:"url"`
			SendResolved bool   `json:"send_resolved"`
			HTTPConfig   struct {
				Authorization struct {
					Type            string
					CredentialsFile string `json:"credentials_file"`
				}
			} `json:"http_config"`
		} `json:"webhook_configs"`
	}
	Route struct {
		Routes []struct {
			Receiver string
			Matchers []string
			Continue bool
		}
	}
}

func (c alertmanagerConfig) check(t *testing.T, tokenFile string) {
	t.Helper()
	if len(c.Receivers) != 1 || len(c.Receivers[0].WebhookConfigs) != 1 {
		t.Fatalf("receivers = %+v", c.Receivers)
	}
	wh := c.Receivers[0].WebhookConfigs[0]
	if wh.URL != "http://themis-core.themis.svc:8080/v1/intake/alertmanager" || !wh.SendResolved ||
		wh.HTTPConfig.Authorization.Type != "Bearer" || wh.HTTPConfig.Authorization.CredentialsFile != tokenFile {
		t.Errorf("webhook = %+v", wh)
	}
	routes := c.Route.Routes
	// Every alert some profile handles, once each, regex-quoted.
	if len(routes) != 1 || routes[0].Receiver != "themis" || !routes[0].Continue || len(routes[0].Matchers) != 1 ||
		routes[0].Matchers[0] != `alertname=~"Custom\.Alert|KubePodCrashLooping|KubePodOOMKilled"` {
		t.Errorf("routes = %+v", routes)
	}
}

func TestAlertmanagerSetup_Refuses(t *testing.T) {
	for name, tc := range map[string]struct {
		intake config.Intake
		query  string
		code   int
	}{
		"a bad namespace":         {config.Intake{URL: "http://x", TokenSecret: "t"}, "?namespace=Not_A_Namespace", http.StatusBadRequest},
		"no address, no chart":    {config.Intake{}, "", http.StatusNotFound},
		"a namespace with a pipe": {config.Intake{URL: "http://x", TokenSecret: "t"}, "?namespace=a%7Cb", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/setup/alertmanager"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			setupAPI(t, tc.intake).ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Errorf("= %d %q, want %d", rec.Code, rec.Body, tc.code)
			}
		})
	}
}
