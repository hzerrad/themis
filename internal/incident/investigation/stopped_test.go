package investigation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

var alertStart = time.Date(2026, 11, 8, 14, 27, 6, 0, time.UTC)

// alert records an alert firing for the incident, as intake does, and, when
// stopped, its end.
func (e *env) alert(t *testing.T, id, sourceID string, stopped bool) {
	t.Helper()
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		err := e.store.RecordFiringAlert(t.Context(), tx, store.FiringAlertArgs{IncidentID: id, Source: "alertmanager", SourceID: sourceID, StartsAt: alertStart})
		if err != nil || !stopped {
			return err
		}
		_, err = e.store.ResolveAlert(t.Context(), tx, store.ResolvedAlertArgs{
			Source: "alertmanager", SourceID: sourceID, StartsAt: alertStart, EndsAt: alertStart.Add(28 * time.Second), ReceivedAt: alertStart.Add(28 * time.Second),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Alerts stop for reasons other than a recovery, such as the pods they were
// about being replaced: an incident whose alerts all stopped stays open while
// its symptoms are true, or can't be told, is decided again later, and
// closes as recovered once they are false.
func TestAlertsStopped_KeepsAFailingIncidentOpen(t *testing.T) {
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
	e.alert(t, id, "fp-a", true)
	worker := func(found check.Truth) *investigation.AlertsStoppedWorker {
		return &investigation.AlertsStoppedWorker{Config: cfg, Store: e.store, Reader: k8s, Checks: symptoms{found}, Words: e.cat.Checks}
	}

	for _, found := range []check.Truth{check.True, check.Unknown} {
		open, err := worker(found).Decide(t.Context(), id)
		if err != nil || !open {
			t.Fatalf("symptom %s: open = %v, %v, want it kept open", found, open, err)
		}
		if got := e.state(t, id); got != incident.AwaitingSuggestion {
			t.Fatalf("symptom %s: state = %s, want it still waiting", found, got)
		}
	}
	// As River runs it: decided again in a minute.
	err := worker(check.True).Work(t.Context(), &river.Job[investigation.AlertsStoppedArgs]{Args: investigation.AlertsStoppedArgs{IncidentID: id}})
	var snooze *rivertype.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != investigation.StoppedRecheckEvery {
		t.Errorf("Work = %v, want it snoozed for %s", err, investigation.StoppedRecheckEvery)
	}

	if open, err := worker(check.False).Decide(t.Context(), id); err != nil || open {
		t.Fatalf("open = %v, %v once the symptom cleared, want it closed", open, err)
	}
	if got := e.state(t, id); got != incident.SelfResolved {
		t.Fatalf("state = %s once the symptom cleared, want recovered", got)
	}
	want := "All of its alerts stopped firing. The checks now find that " +
		e.cat.Checks.Found(check.Result{Name: "oom_killed_new_revision", Value: check.False, Reason: "found false"}) + "."
	if _, reason := e.path(t, id); reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

// An alert that fires again, or another of the incident's, keeps it as it
// is: nothing is decided while one fires.
func TestAlertsStopped_NothingWhileAnAlertFires(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "rollout-regression", e.target)
	e.alert(t, id, "fp-a", true)
	e.alert(t, id, "fp-b", false)
	w := &investigation.AlertsStoppedWorker{Config: e.worker.Config, Store: e.store, Reader: k8s, Checks: symptoms{check.False}, Words: e.cat.Checks}
	if open, err := w.Decide(t.Context(), id); err != nil || open {
		t.Fatalf("open = %v, %v, want nothing to decide", open, err)
	}
	if got := e.state(t, id); got != incident.Received {
		t.Errorf("state = %s, want it untouched", got)
	}
}

// Alerts that stop because their workload was deleted end the incident as
// such, saying so.
func TestAlertsStopped_WorkloadGone(t *testing.T) {
	e := newEnv(t)
	p := profile()
	p.Symptoms = []string{"oom_killed_new_revision"}
	deleted := e.target
	deleted.Name = "deleted"
	id := e.newIncident(t, "rollout-regression", deleted)
	e.alert(t, id, "fp-a", true)
	w := &investigation.AlertsStoppedWorker{Config: config.Config{Profiles: []config.Profile{p}}, Store: e.store, Reader: k8s, Checks: symptoms{check.True}, Words: e.cat.Checks}
	if err := w.Work(t.Context(), &river.Job[investigation.AlertsStoppedArgs]{Args: investigation.AlertsStoppedArgs{IncidentID: id}}); err != nil {
		t.Fatalf("Work = %v, want the incident ended", err)
	}
	if got := e.state(t, id); got != incident.Canceled {
		t.Errorf("state = %s, want canceled", got)
	}
}

// Closing while a fix awaits approval cancels the fix: nobody should approve
// it any more, and it must not block the next action on the target.
func TestAlertsStopped_CancelsAPendingFix(t *testing.T) {
	e := newEnv(t)
	id := e.newIncident(t, "rollout-regression", e.target)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		if err := e.store.TransitionPath(t.Context(), tx, id, "test", "test", incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval); err != nil {
			return err
		}
		p, _, err := e.store.InsertProposal(t.Context(), tx, store.Proposal{IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p", Capability: "c", CapabilityVersion: "1", Params: map[string]any{}, Status: store.ProposalAccepted})
		if err != nil {
			return err
		}
		_, err = e.store.InsertAction(t.Context(), tx, store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "c", Target: digest.TargetIdentity{UID: "u"},
			Params: map[string]any{}, BlastRadius: map[string]any{}, Constraints: map[string]any{}, State: store.ActionAwaitingApproval, Digest: "d",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.alert(t, id, "fp-a", true)
	w := &investigation.AlertsStoppedWorker{Config: e.worker.Config, Store: e.store, Reader: k8s, Checks: symptoms{check.False}, Words: e.cat.Checks}
	if _, err := w.Decide(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, id); got != incident.SelfResolved {
		t.Errorf("state = %s, want self_resolved", got)
	}
	if _, reason := e.path(t, id); reason != "All of its alerts stopped firing." {
		t.Errorf("reason = %q, want only that its alerts stopped: its checks never found its failure", reason)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1 AND state = 'canceled'`, id); n != 1 {
		t.Error("the pending action was not canceled")
	}
}
