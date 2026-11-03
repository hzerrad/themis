package checks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/hzerrad/themis/internal/checks"
	"github.com/hzerrad/themis/internal/config"
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

// healthy is an install where everything works.
func healthy() (*checks.Checker, *fakeStore) {
	s := &fakeStore{
		heartbeats: map[string]store.Heartbeat{
			store.HeartbeatInvestigator: {SeenAt: now.Add(-10 * time.Second), Detail: "worker-1"},
			store.HeartbeatAlertmanager: {SeenAt: now.Add(-time.Minute), Detail: "themis"},
		},
		outcome:  &store.InvestigationOutcome{Completed: true, Model: "qwen3:8b", At: now.Add(-time.Hour)},
		accounts: true, finished: true,
	}
	return &checks.Checker{
		Config: config.Config{EnrolledNamespaces: []string{"shop", "payments"}}, Store: s, Access: fakeAccess{},
		Executor: "themis-executor:9091", Dial: func(context.Context, string) error { return nil },
		Now: func() time.Time { return now },
	}, s
}

func byName(t *testing.T, c *checks.Checker) map[string]checks.Check {
	t.Helper()
	out := map[string]checks.Check{}
	for _, ch := range c.Checks(t.Context()) {
		out[ch.Name] = ch
		if ch.State != checks.OK && ch.Fix == "" && ch.State != checks.Waiting {
			t.Errorf("%s is %s but says no fix", ch.Name, ch.State)
		}
		if !strings.HasSuffix(ch.Summary, ".") || ch.Summary[0] < 'A' || ch.Summary[0] > 'Z' {
			t.Errorf("%s's summary %q isn't a sentence", ch.Name, ch.Summary)
		}
	}
	return out
}

func TestChecks_Healthy(t *testing.T) {
	c, _ := healthy()
	got := byName(t, c)
	for _, name := range []string{"Database", "Investigator", "Model", "Alerts", "Namespaces", "Fixes", "Sign-in"} {
		if got[name].State != checks.OK {
			t.Errorf("%s = %+v, want ok", name, got[name])
		}
	}
	if s := got["Model"].Summary; !strings.Contains(s, "qwen3:8b") {
		t.Errorf("model summary %q doesn't name the model", s)
	}
	if s := got["Namespaces"].Summary; s != "Themis can read shop and payments." {
		t.Errorf("namespaces summary = %q", s)
	}
}

func TestChecks_NoDatabase(t *testing.T) {
	c, s := healthy()
	s.pingErr = errors.New("connection refused")
	got := c.Checks(t.Context())
	if len(got) != 1 || got[0].Name != "Database" || got[0].State != checks.Problem {
		t.Errorf("checks without a database = %+v, want only the database's problem", got)
	}
}

func TestChecks_Problems(t *testing.T) {
	for name, tc := range map[string]struct {
		break_ func(*checks.Checker, *fakeStore)
		check  string
		state  checks.State
		says   string
	}{
		"investigator never seen": {
			func(_ *checks.Checker, s *fakeStore) { delete(s.heartbeats, store.HeartbeatInvestigator) },
			"Investigator", checks.Problem, "hasn't asked",
		},
		"investigator gone quiet": {func(_ *checks.Checker, s *fakeStore) {
			s.heartbeats[store.HeartbeatInvestigator] = store.Heartbeat{SeenAt: now.Add(-5 * time.Minute)}
		}, "Investigator", checks.Problem, "stopped"},
		"model never used": {func(_ *checks.Checker, s *fakeStore) { s.outcome = nil }, "Model", checks.Waiting, "Not used yet"},
		"model unreachable": {func(_ *checks.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "model_unavailable", At: now}
		}, "Model", checks.Problem, "couldn't be reached"},
		"model's format": {func(_ *checks.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "invalid_model_output", At: now}
		}, "Model", checks.Problem, "format"},
		"token budget": {func(_ *checks.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "budget_exhausted", At: now}
		}, "Model", checks.Warn, "budget"},
		"unknown failure": {func(_ *checks.Checker, s *fakeStore) {
			s.outcome = &store.InvestigationOutcome{FailureKind: "internal", Failure: "The investigation failed because the investigator hit an internal error: boom", At: now}
		}, "Model", checks.Problem, "internal error"},
		"no alerts yet": {
			func(_ *checks.Checker, s *fakeStore) { delete(s.heartbeats, store.HeartbeatAlertmanager) },
			"Alerts", checks.Waiting, "No alert",
		},
		"wrong token": {func(_ *checks.Checker, s *fakeStore) {
			s.heartbeats[store.HeartbeatAlertmanagerRefused] = store.Heartbeat{SeenAt: now}
		}, "Alerts", checks.Problem, "wrong token"},
		"a missing namespace": {func(c *checks.Checker, _ *fakeStore) {
			c.Access = fakeAccess{"payments/list pods": true, "payments/get deployments": true}
		}, "Namespaces", checks.Problem, "In payments, Themis can't list pods or read deployments."},
		"no logs": {
			func(c *checks.Checker, _ *fakeStore) { c.Access = fakeAccess{"shop/get pods/log": true} },
			"Namespaces", checks.Warn, "read container logs",
		},
		"fixes off": {func(c *checks.Checker, _ *fakeStore) { c.Executor = "" }, "Fixes", checks.Off, "never applies"},
		"executor down": {func(c *checks.Checker, _ *fakeStore) {
			c.Dial = func(context.Context, string) error { return errors.New("refused") }
		}, "Fixes", checks.Problem, "can't reach the executor"},
		"setup open": {func(_ *checks.Checker, s *fakeStore) { s.finished = false }, "Sign-in", checks.Warn, "port-forward"},
		"token only": {func(_ *checks.Checker, s *fakeStore) { s.accounts = false }, "Sign-in", checks.Warn, "emergency token"},
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

type recorder struct{ beats []string }

func (r *recorder) Beat(_ context.Context, component, detail string, _ time.Time) error {
	r.beats = append(r.beats, component+":"+detail)
	return nil
}

// A heartbeat is written at most every half minute.
func TestHeartbeat_Throttles(t *testing.T) {
	r := &recorder{}
	clock := now
	h := &checks.Heartbeat{Store: r, Component: "investigator", Now: func() time.Time { return clock }}
	h.Beat(t.Context(), "a")
	clock = clock.Add(10 * time.Second)
	h.Beat(t.Context(), "b")
	clock = clock.Add(25 * time.Second)
	h.Beat(t.Context(), "c")
	if want := []string{"investigator:a", "investigator:c"}; strings.Join(r.beats, ",") != strings.Join(want, ",") {
		t.Errorf("beats = %v, want %v", r.beats, want)
	}
	var none *checks.Heartbeat
	none.Beat(t.Context(), "nothing") // a nil heartbeat records nothing and doesn't panic
}
