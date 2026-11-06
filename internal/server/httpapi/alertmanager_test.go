package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/incident/intake/alertmanager"
	"github.com/hzerrad/themis/internal/incident/intake/rules"
	"github.com/hzerrad/themis/internal/server/httpapi"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// alertmanagerAPI serves the alerting settings, for the admin token and an
// approver signed in with the session approverSession.
func alertmanagerAPI(t *testing.T, install config.Alerting) http.Handler {
	t.Helper()
	s, _ := pgtest.NewStore(t)
	se := store.Session{Subject: "a", Username: "approver@example.com", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateSession(t.Context(), approverSession, se); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Profiles: []config.Profile{{Match: config.Match{AlertNames: []string{"KubePodCrashLooping"}}}}}
	mux := http.NewServeMux()
	(&httpapi.API{
		Store: s, TokenFile: tokenFile(t), Config: cfg, Sessions: s, OIDC: true,
		Access: []config.AccessBinding{{Role: config.RoleApprover, Users: []string{"approver@example.com"}}},
		Alertmanager: &alertmanager.Connection{
			Install: install.Alertmanager, Addresses: &settings.Settings{Store: s, InstallAlerting: install}, Reads: s,
		},
	}).Register(mux)
	return mux
}

const approverSession = "session-approver"

// asApprover sends a request as a signed-in approver.
func asApprover(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "themis_session", Value: approverSession, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	req.Header.Set("X-Themis-Request", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// view returns the alerting settings as the Setup page reads them.
func view(t *testing.T, h http.Handler) settings.AlertmanagerView {
	t.Helper()
	var v settings.AlertmanagerView
	rec := send(t, h, http.MethodGet, "/v1/settings/alertmanager", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("GET = %d %q, %v", rec.Code, rec.Body, err)
	}
	return v
}

// An admin switches between poll and push mode on the Setup page, keeping
// the address; nobody else may. A mode Helm set can't be switched.
func TestAlertmanagerSettings_Mode(t *testing.T) {
	const address = "http://alertmanager.observability:9093"
	h := alertmanagerAPI(t, config.Alerting{Alertmanager: config.Alertmanager{Address: "http://monitoring-alertmanager.monitoring.svc:9093", Found: true}})
	if v := view(t, h); v.Mode != config.PollMode || v.ModeByInstall {
		t.Fatalf("by default = %+v, want poll mode, switchable", v)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"address": address}); rec.Code != http.StatusOK {
		t.Fatalf("PUT address = %d %q", rec.Code, rec.Body)
	}
	if rec := asApprover(t, h, http.MethodPut, "/v1/settings/alertmanager", `{"mode": "push"}`); rec.Code != http.StatusForbidden {
		t.Errorf("an approver's switch = %d %q, want 403", rec.Code, rec.Body)
	}
	if v := view(t, h); v.Mode != config.PollMode {
		t.Errorf("after an approver's switch = %+v, want poll mode", v)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"mode": "push"}); rec.Code != http.StatusOK {
		t.Fatalf("an admin's switch = %d %q", rec.Code, rec.Body)
	}
	if v := view(t, h); v.Mode != config.PushMode || v.ModeSince.IsZero() || v.Address != address {
		t.Errorf("switched to push = %+v, want push mode, since now, the address kept", v)
	}
	if rec := send(t, h, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"mode": "pull"}); rec.Code != http.StatusBadRequest ||
		rec.Body.String() != "The mode must be poll or push.\n" {
		t.Errorf("an unknown mode = %d %q, want a 400 sentence", rec.Code, rec.Body)
	}

	fixed := alertmanagerAPI(t, config.Alerting{Mode: config.PollMode})
	if rec := send(t, fixed, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"mode": "push"}); rec.Code != http.StatusConflict {
		t.Errorf("switching the mode Helm set = %d %q, want 409", rec.Code, rec.Body)
	}
	if v := view(t, fixed); v.Mode != config.PollMode || !v.ModeByInstall {
		t.Errorf("poll mode set in Helm = %+v", v)
	}
	push := alertmanagerAPI(t, config.Alerting{Alertmanager: config.Alertmanager{Disabled: true}})
	if rec := send(t, push, http.MethodPut, "/v1/settings/alertmanager", map[string]string{"address": address}); rec.Code != http.StatusBadRequest ||
		!strings.HasPrefix(rec.Body.String(), "Push mode is set in the Helm values") {
		t.Errorf("an address in push mode set in Helm = %d %q, want a 400 sentence", rec.Code, rec.Body)
	}
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
	h := alertmanagerAPI(t, config.Alerting{Alertmanager: config.Alertmanager{Address: "http://monitoring-alertmanager.monitoring.svc:9093", Found: true}})

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

	installed := alertmanagerAPI(t, config.Alerting{Alertmanager: config.Alertmanager{Address: am.URL}})
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
