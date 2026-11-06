package investigation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/workload"
)

// symptoms stands in for the catalog: every symptom asked about is found.
type symptoms struct{ found check.Truth }

func (s symptoms) Recheck(_ context.Context, _ client.Reader, _ *workload.Workload, names []string, _ time.Duration, _ time.Time) []check.Result {
	var out []check.Result
	for _, n := range names {
		out = append(out, check.Result{Name: n, Value: s.found, Reason: "found " + s.found.String()})
	}
	return out
}

// No timer closes an incident whose failure goes on: past its window, one
// waiting for a suggested fix keeps waiting while its symptoms are true, or
// can't be told, and closes as recovered once they are false.
func TestCloseUndecided_KeepsAFailingIncidentOpen(t *testing.T) {
	e := newEnv(t)
	p := profile()
	p.Symptoms = []string{"oom_killed_new_revision"}
	cfg := e.worker.Config.Current()
	cfg.Profiles = []config.Profile{p}
	cfg.Investigators.Disabled = true
	e.worker.Config = cfg
	id := e.newIncident(t, "rollout-regression", e.target)
	if err := e.worker.Prepare(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	sweep := func(found check.Truth) {
		w := &investigation.CloseUndecidedWorker{
			Config: cfg, Store: e.store, Reader: k8s, Checks: symptoms{found}, Words: e.cat.Checks,
			Now: func() time.Time { return time.Now().Add(time.Hour) },
		}
		if err := w.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, found := range []check.Truth{check.True, check.Unknown} {
		sweep(found)
		if got := e.state(t, id); got != incident.AwaitingSuggestion {
			t.Fatalf("symptom %s: state = %s, want it still waiting", found, got)
		}
	}
	sweep(check.False)
	if got := e.state(t, id); got != incident.SelfResolved {
		t.Fatalf("state = %s once the symptom cleared, want recovered", got)
	}
	if _, reason := e.path(t, id); reason != "Recovered before a fix. The checks now find that "+e.cat.Checks.Found(check.Result{Name: "oom_killed_new_revision", Value: check.False, Reason: "found false"})+"." {
		t.Errorf("reason = %q", reason)
	}
}

// A person closes an incident waiting for a suggested fix, with their
// reason; one that isn't waiting can't be closed this way.
func TestClose_ByAPerson(t *testing.T) {
	e := newEnv(t)
	id := e.waiting(t)
	if err := investigation.Close(t.Context(), e.store, id, "alice", "handled by the vendor"); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed", got)
	}
	if _, reason := e.path(t, id); reason != "Closed by alice. Reason: handled by the vendor." {
		t.Errorf("reason = %q", reason)
	}
	var refused *investigation.Refusal
	if err := investigation.Close(t.Context(), e.store, id, "alice", ""); !errors.As(err, &refused) {
		t.Errorf("closing a closed incident: %v, want a refusal", err)
	}
}
