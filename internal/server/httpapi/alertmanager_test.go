package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/incident/intake/rules"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

func alertmanagerAPI(t *testing.T, install config.Alertmanager) http.Handler {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	cfg := config.Config{Profiles: []config.Profile{{Match: config.Match{AlertNames: []string{"KubePodCrashLooping"}}}}}
	mux := http.NewServeMux()
	(&httpapi.API{Store: s, TokenFile: tokenFile(t), Config: cfg, Alertmanager: &alertmanager.Connection{
		Install: install, Addresses: &settings.Settings{Store: s, InstallAlertmanager: install}, Reads: s,
	}}).Register(mux)
	return mux
}

// An Alertmanager listing one KubePodCrashLooping alert.
func fakeAlertmanager(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/status" {
			_, _ = w.Write([]byte(`{"uptime": "2026-11-06T09:00:00Z", "versionInfo": {"version": "0.34.1"}}`))
			return
		}
		_, _ = w.Write([]byte(`[{"fingerprint": "a", "labels": {"alertname": "KubePodCrashLooping"}, "startsAt": "2026-11-06T10:00:00Z", "status": {"state": "active"}}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The Setup page replaces the address found at install, and tests one.
func TestAlertmanagerSettings(t *testing.T) {
	am := fakeAlertmanager(t)
	h := alertmanagerAPI(t, config.Alertmanager{Address: "http://monitoring-alertmanager.monitoring.svc:9093", Found: true})

	var v settings.AlertmanagerView
	rec := send(t, h, http.MethodGet, "/v1/settings/alertmanager", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.From != settings.FromFound {
		t.Fatalf("GET = %d %q, %v", rec.Code, rec.Body, err)
	}
	rec = send(t, h, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"address": am.URL})
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || rec.Code != http.StatusOK || v.Address != am.URL || v.From != settings.FromSetup {
		t.Errorf("PUT = %d %q, %v", rec.Code, rec.Body, err)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"address": "alertmanager:9093"}); rec.Code != http.StatusBadRequest ||
		!strings.HasPrefix(rec.Body.String(), "Alertmanager's address must be an http or https URL") {
		t.Errorf("a bad address = %d %q, want a 400 sentence", rec.Code, rec.Body)
	}

	var got alertmanager.TestResult
	rec = send(t, h, http.MethodPost, "/v1/setup/alertmanager/test", map[string]string{"address": ""})
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Active != 1 || len(got.Firing) != 1 || got.Firing[0].Name != "KubePodCrashLooping" {
		t.Errorf("test = %d %q, %v", rec.Code, rec.Body, err)
	}
	if rec := send(t, h, http.MethodPost, "/v1/setup/alertmanager/test", map[string]string{"address": "http://127.0.0.1:1"}); rec.Code != http.StatusBadGateway ||
		!strings.HasPrefix(rec.Body.String(), "Alertmanager at http://127.0.0.1:1 doesn't answer") {
		t.Errorf("test of an address nothing answers = %d %q, want 502", rec.Code, rec.Body)
	}

	installed := alertmanagerAPI(t, config.Alertmanager{Address: am.URL})
	if rec := send(t, installed, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"address": "http://other:9093"}); rec.Code != http.StatusConflict {
		t.Errorf("changing the install's address = %d %q, want 409", rec.Code, rec.Body)
	}
}

// missingOOM is a Prometheus with every rule but KubePodOOMKilled's.
type missingOOM struct{}

func (missingOOM) Check(_ context.Context, answered []string) (rules.Result, error) {
	res := rules.Result{Address: "http://prometheus:9090", Exist: []string{}, Missing: []string{}}
	for _, name := range answered {
		if name == "KubePodOOMKilled" {
			res.Missing = append(res.Missing, name)
		} else {
			res.Exist = append(res.Exist, name)
		}
	}
	return res, nil
}

// The Setup page says which alerts have rules in Prometheus, and gives the
// rules Themis ships for those missing: for every alert, without Prometheus.
func TestAlertRules(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	cfg := config.Config{Profiles: []config.Profile{{Match: config.Match{AlertNames: []string{"KubePodCrashLooping", "KubePodOOMKilled", "Custom.Alert"}}}}}
	get := func(r httpapi.AlertRules) map[string]any {
		t.Helper()
		mux := http.NewServeMux()
		(&httpapi.API{Store: s, TokenFile: tokenFile(t), Config: cfg, Rules: r}).Register(mux)
		rec := send(t, mux, http.MethodGet, "/v1/setup/alert-rules", nil)
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("GET = %d %q", rec.Code, rec.Body)
		}
		return v
	}

	v := get(nil)
	if v["checked"] != false || v["standard"] != true || fmt.Sprint(v["unshipped"]) != "[Custom.Alert]" ||
		!strings.Contains(v["ruleFile"].(string), "alert: KubePodCrashLooping") || !strings.Contains(v["ruleFile"].(string), "alert: KubePodOOMKilled") {
		t.Errorf("without Prometheus = %v", v)
	}
	v = get(missingOOM{})
	if v["checked"] != true || fmt.Sprint(v["missing"]) != "[KubePodOOMKilled]" || v["standard"] != false ||
		strings.Contains(v["ruleFile"].(string), "KubePodCrashLooping") || !strings.Contains(v["prometheusRule"].(string), "kind: PrometheusRule") {
		t.Errorf("with KubePodOOMKilled missing = %v", v)
	}
}
