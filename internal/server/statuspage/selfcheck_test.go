package statuspage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/config/settings"
	"github.com/hzerrad/themis/internal/server/statuspage"
	"github.com/hzerrad/themis/internal/store"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

type fakeStore struct {
	pingErr    error
	heartbeats map[string]store.Heartbeat
	outcome    *store.InvestigationOutcome
	accounts   bool
	finished   bool
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) LastHeartbeat(_ context.Context, c string) (store.Heartbeat, error) {
	h, ok := f.heartbeats[c]
	if !ok {
		return store.Heartbeat{}, store.ErrNotFound
	}
	return h, nil
}

func (f *fakeStore) LastInvestigationOutcome(context.Context) (store.InvestigationOutcome, error) {
	if f.outcome == nil {
		return store.InvestigationOutcome{}, store.ErrNotFound
	}
	return *f.outcome, nil
}

func (f *fakeStore) HasAccounts(context.Context) (bool, error)   { return f.accounts, nil }
func (f *fakeStore) SetupFinished(context.Context) (bool, error) { return f.finished, nil }

// fakeAccess allows everything but what's denied, as "namespace/verb resource[/subresource]".
type fakeAccess map[string]bool

func (f fakeAccess) Can(_ context.Context, a authorizationv1.ResourceAttributes) (bool, error) {
	key := a.Namespace + "/" + a.Verb + " " + a.Resource
	if a.Subresource != "" {
		key += "/" + a.Subresource
	}
	return !f[key], nil
}

// fakeMetrics are metrics sources in the health given.
type fakeMetrics []metrics.Health

func (f fakeMetrics) Health(context.Context) []metrics.Health { return f }

// healthy is an install where everything works.
func healthy() (*statuspage.Checker, *fakeStore) {
	s := &fakeStore{
		heartbeats: map[string]store.Heartbeat{
			store.HeartbeatInvestigator: {SeenAt: now.Add(-10 * time.Second), Detail: "worker-1"},
			store.HeartbeatAlertmanager: {SeenAt: now.Add(-time.Minute), Detail: "themis"},
		},
		outcome:  &store.InvestigationOutcome{Completed: true, Model: "qwen3:8b", At: now.Add(-time.Hour)},
		accounts: true, finished: true,
	}
	return &statuspage.Checker{
		Config: config.Config{
			EnrolledNamespaces: []string{"shop", "payments"},
			Metrics:            config.Metrics{Prometheus: &config.Prometheus{Address: "http://prometheus:9090"}},
		},
		Store: s, Access: fakeAccess{},
		Metrics:  fakeMetrics{{Source: metrics.Prometheus, History: true}, {Source: metrics.Kubernetes}},
		Executor: "themis-executor:9091", Dial: func(context.Context, string) error { return nil },
		InitialAdmin: true, Now: func() time.Time { return now },
	}, s
}

func byName(t *testing.T, c *statuspage.Checker) map[string]statuspage.Check {
	t.Helper()
	out := map[string]statuspage.Check{}
	for _, ch := range c.Checks(t.Context()) {
		out[ch.Name] = ch
		if ch.State != statuspage.OK && ch.Fix == "" && ch.State != statuspage.Waiting {
			t.Errorf("%s is %s but says no fix", ch.Name, ch.State)
		}
		// A model's name opens its finding as it is written, in any case.
		upper := ch.Name == "Model" && ch.State == statuspage.OK || ch.Summary[0] >= 'A' && ch.Summary[0] <= 'Z'
		if !strings.HasSuffix(ch.Summary, ".") || !upper {
			t.Errorf("%s's summary %q isn't a sentence", ch.Name, ch.Summary)
		}
	}
	return out
}

func TestChecks_Healthy(t *testing.T) {
	c, _ := healthy()
	got := byName(t, c)
	for _, name := range []string{"Database", "Investigator", "Model", "Alerts", "Namespaces", "Metrics", "Fixes", "Sign-in"} {
		if got[name].State != statuspage.OK {
			t.Errorf("%s = %+v, want ok", name, got[name])
		}
	}
	if s := got["Model"].Summary; !strings.Contains(s, "qwen3:8b") {
		t.Errorf("model summary %q doesn't name the model", s)
	}
	if s := got["Namespaces"].Summary; s != "Readable: shop, payments." {
		t.Errorf("namespaces summary = %q", s)
	}
	if s := got["Metrics"].Summary; s != "Read from Prometheus at http://prometheus:9090 and the Kubernetes metrics API." {
		t.Errorf("metrics summary = %q", s)
	}
}

func TestChecks_NoDatabase(t *testing.T) {
	c, s := healthy()
	s.pingErr = errors.New("connection refused")
	got := c.Checks(t.Context())
	if len(got) != 1 || got[0].Name != "Database" || got[0].State != statuspage.Problem {
		t.Errorf("checks without a database = %+v, want only the database's problem", got)
	}
}

func TestChecks_Problems(t *testing.T) {
	for name, tc := range map[string]struct {
		break_ func(*statuspage.Checker, *fakeStore)
		check  string
		state  statuspage.State
		says   string
	}{
		"investigator never seen": {
			func(_ *statuspage.Checker, s *fakeStore) { delete(s.heartbeats, store.HeartbeatInvestigator) },
			"Investigator", statuspage.Problem, "Not connected",
		},
		"investigator gone quiet": {func(_ *statuspage.Checker, s *fakeStore) {
			s.heartbeats[store.HeartbeatInvestigator] = store.Heartbeat{SeenAt: now.Add(-5 * time.Minute)}
		}, "Investigator", statuspage.Problem, "Disconnected"},
		"model never used": {func(_ *statuspage.Checker, s *fakeStore) { s.outcome = nil }, "Model", statuspage.Waiting, "Not used yet"},
		"model unreachable": {func(_ *statuspage.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "model_unavailable", At: now}
		}, "Model", statuspage.Problem, "Unreachable"},
		"model's format": {func(_ *statuspage.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "invalid_model_output", At: now}
		}, "Model", statuspage.Problem, "format"},
		"token budget": {func(_ *statuspage.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "budget_exhausted", At: now}
		}, "Model", statuspage.Warn, "budget"},
		"unknown failure": {func(_ *statuspage.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "internal", Failure: "The investigation failed because the investigator hit an internal error: boom", At: now}
		}, "Model", statuspage.Problem, "internal error"},
		"no alerts yet": {
			func(_ *statuspage.Checker, s *fakeStore) { delete(s.heartbeats, store.HeartbeatAlertmanager) },
			"Alerts", statuspage.Waiting, "No alert",
		},
		"wrong token": {func(_ *statuspage.Checker, s *fakeStore) {
			s.heartbeats[store.HeartbeatAlertmanagerRefused] = store.Heartbeat{SeenAt: now}
		}, "Alerts", statuspage.Problem, "Refused: wrong intake token."},
		"a missing namespace": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Access = fakeAccess{"payments/list pods": true, "payments/get deployments": true}
		}, "Namespaces", statuspage.Problem, "Not allowed in payments: list pods or read deployments."},
		"no logs": {
			func(c *statuspage.Checker, _ *fakeStore) { c.Access = fakeAccess{"shop/get pods/log": true} },
			"Namespaces", statuspage.Warn, "read container logs",
		},
		"no metrics source": {func(c *statuspage.Checker, _ *fakeStore) { c.Metrics = nil }, "Metrics", statuspage.Warn, "No metrics source."},
		"metrics without history": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Metrics = fakeMetrics{{Source: metrics.Kubernetes}}
		}, "Metrics", statuspage.OK, "No history: peak memory and CPU throttling are unknown."},
		"no metrics-server": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Metrics = fakeMetrics{{Source: metrics.Kubernetes, Err: metrics.ErrNotServed}}
		}, "Metrics", statuspage.Warn, "No metrics source."},
		"metrics API failing": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Metrics = fakeMetrics{{Source: metrics.Kubernetes, Err: errors.New("list pod metrics in shop: forbidden")}}
		}, "Metrics", statuspage.Warn, "List pod metrics in shop: forbidden."},
		"prometheus down": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Metrics = fakeMetrics{{Source: metrics.Prometheus, History: true, Err: errors.New("prometheus at http://prometheus:9090 answered 502")}, {Source: metrics.Kubernetes}}
		}, "Metrics", statuspage.Problem, "Read from the Kubernetes metrics API. Prometheus at http://prometheus:9090 answered 502."},
		"fixes off": {func(c *statuspage.Checker, _ *fakeStore) { c.Executor = "" }, "Fixes", statuspage.Off, "never applied"},
		"executor down": {func(c *statuspage.Checker, _ *fakeStore) {
			c.Dial = func(context.Context, string) error { return errors.New("refused") }
		}, "Fixes", statuspage.Problem, "Executor unreachable"},
		"no model set":    {func(c *statuspage.Checker, _ *fakeStore) { c.Model = fixedView{} }, "Model", statuspage.Problem, "Not set"},
		"no investigator": {func(c *statuspage.Checker, _ *fakeStore) { c.NoInvestigator = true }, "Investigator", statuspage.Off, "Not installed"},
		"no model":        {func(c *statuspage.Checker, _ *fakeStore) { c.NoInvestigator = true }, "Model", statuspage.Off, "no investigator"},
		"setup open":      {func(_ *statuspage.Checker, s *fakeStore) { s.finished = false }, "Sign-in", statuspage.Warn, "The initial admin password grants admin."},
		"setup finished":  {func(*statuspage.Checker, *fakeStore) {}, "Sign-in", statuspage.OK, "Through Themis accounts. The initial admin password is retired."},
		"token only":      {func(_ *statuspage.Checker, s *fakeStore) { s.accounts = false }, "Sign-in", statuspage.Warn, "break-glass token"},
	} {
		t.Run(name, func(t *testing.T) {
			c, s := healthy()
			tc.break_(c, s)
			got := byName(t, c)[tc.check]
			if got.State != tc.state || !strings.Contains(got.Summary, tc.says) {
				t.Errorf("%s = %+v, want %s saying %q", tc.check, got, tc.state, tc.says)
			}
		})
	}
}

type fixedView settings.ModelView

func (f fixedView) View(context.Context) (settings.ModelView, error) {
	return settings.ModelView(f), nil
}

type recorder struct{ beats []string }

func (r *recorder) Beat(_ context.Context, component, detail string, _ time.Time) error {
	r.beats = append(r.beats, component+":"+detail)
	return nil
}

// A heartbeat is written at most every half minute.
func TestHeartbeat_Throttles(t *testing.T) {
	r := &recorder{}
	clock := now
	h := &statuspage.Heartbeat{Store: r, Component: "investigator", Now: func() time.Time { return clock }}
	h.Beat(t.Context(), "a")
	clock = clock.Add(10 * time.Second)
	h.Beat(t.Context(), "b")
	clock = clock.Add(25 * time.Second)
	h.Beat(t.Context(), "c")
	if want := []string{"investigator:a", "investigator:c"}; strings.Join(r.beats, ",") != strings.Join(want, ",") {
		t.Errorf("beats = %v, want %v", r.beats, want)
	}
	var none *statuspage.Heartbeat
	none.Beat(t.Context(), "nothing") // a nil heartbeat records nothing and doesn't panic
}
