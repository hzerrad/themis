package governance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/policy"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// restoring governs a rollback to revision 1, records it as applied without
// helping, and leaves the incident restoring. It returns the incident and the
// fix.
func (e *env) restoring(t *testing.T) (string, store.Action) {
	t.Helper()
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(check.True))
	e.govern(t, p)
	fix := e.action(t, id)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET state = 'succeeded' WHERE id = $1`, fix.ID); err != nil {
		t.Fatal(err)
	}
	path := []incident.State{incident.AwaitingApproval, incident.Authorized, incident.Executing, incident.Verifying, incident.Restoring}
	for i := 1; i < len(path); i++ {
		if err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
			return e.store.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// What the rollback did: revision 1's template is current again, as
	// revision 3.
	live, err := rollback.Read(t.Context(), k8s, e.target, 1)
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template = live.Desired
	d.Annotations["deployment.kubernetes.io/revision"] = "3"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	return id, fix
}

func (e *env) restore(t *testing.T, fix store.Action) {
	t.Helper()
	if err := e.worker.Restore(t.Context(), fix.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

func (e *env) actions(t *testing.T, id string) []store.Action {
	t.Helper()
	actions, err := e.store.IncidentActions(t.Context(), e.store.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

func (e *env) lastReason(t *testing.T, id string) string {
	t.Helper()
	trail, err := e.store.Transitions(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return trail[len(trail)-1].Reason
}

// The fix didn't help: Themis puts back the revision it replaced, as an
// action policy authorized on its own, and hands it to the executor.
func TestRestore_PutsBackWhatTheFixReplaced(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	e.restore(t, fix)

	actions := e.actions(t, id)
	if len(actions) != 2 {
		t.Fatalf("%d actions, want the fix and its restore", len(actions))
	}
	r := actions[1]
	if r.Params["restores"] != fix.ID || fmt.Sprint(r.Params["revision"]) != "2" {
		t.Errorf("restore params = %v, want revision 2, undoing %s", r.Params, fix.ID)
	}
	if r.State != store.ActionAuthorized || r.Decision != string(policy.Allow) || r.ExpiresAt != nil {
		t.Errorf("restore = %s, %s; want authorized by policy, with nothing to approve", r.State, r.Decision)
	}
	if got := e.state(t, id); got != incident.Restoring {
		t.Errorf("state = %s, want restoring until it is applied", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2`, governance.ExecuteArgs{}.Kind(), r.ID); n != 1 {
		t.Errorf("%d execute jobs for the restore, want 1", n)
	}
}

// A repeated job does nothing more.
func TestRestore_Idempotent(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	e.restore(t, fix)
	e.restore(t, fix)
	if n := len(e.actions(t, id)); n != 2 {
		t.Errorf("%d actions after two runs, want 2", n)
	}
}

// When policy forbids putting things back, or asks a person to approve it,
// Themis leaves its change in place, hands the incident over, and counts the
// decision.
func TestRestore_PolicyEscalates(t *testing.T) {
	for _, tt := range []struct {
		decision string
		want     string
	}{
		{config.DecisionRequireApproval, "Policy rule restore-rule requires a person's approval. Putting the workload back needs a person, so Themis left its change in place."},
		{config.DecisionDeny, "Policy rule restore-rule forbids this fix. Themis left its change in place."},
	} {
		t.Run(tt.decision, func(t *testing.T) {
			e := newEnv(t)
			p := profile("rollout-regression")
			p.Policy.Rules = append([]config.PolicyRule{{Name: "restore-rule", When: `action.restore == "true"`, Decision: tt.decision}}, p.Policy.Rules...)
			w := &governance.Worker{Config: config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}, Store: e.store, Reader: k8s, Fixes: catalog.Fixes()}
			w.Now = e.worker.Now
			id, fix := e.restoring(t)
			e.worker = w
			counted := decisions(t, tt.decision)
			e.restore(t, fix)

			if got := e.state(t, id); got != incident.Escalated || e.lastReason(t, id) != tt.want {
				t.Errorf("%s: %q; want escalated, saying %q", got, e.lastReason(t, id), tt.want)
			}
			if n := len(e.actions(t, id)); n != 1 {
				t.Errorf("%d actions, want only the fix", n)
			}
			if got := counted(); got != 1 {
				t.Errorf("counted %v %s decisions, want 1", got, tt.decision)
			}
		})
	}
}

// A restore job for an incident that has left restoring, say because a person
// took it over, changes nothing.
func TestRestore_OnlyWhileRestoring(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	if err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		return e.store.Transition(t.Context(), tx, store.TransitionArgs{ID: id, From: incident.Restoring, To: incident.Escalated, Reason: "taken over", Actor: "test"})
	}); err != nil {
		t.Fatal(err)
	}
	e.restore(t, fix)
	if n := len(e.actions(t, id)); n != 1 {
		t.Errorf("%d actions, want only the fix", n)
	}
	if got := e.lastReason(t, id); got != "taken over" {
		t.Errorf("last reason = %q, want the person's", got)
	}
}

// A fix another incident has in flight on the same workload comes first:
// Themis never runs two at once, so it leaves its change and hands over.
func TestRestore_YieldsToAnotherFix(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	cfg := e.worker.Config.Current()
	cfg.Profiles = append(cfg.Profiles, profile("other"))
	e.worker.Config = cfg
	other, p := e.diagnosed(t, "other", 2, predicates)
	e.govern(t, p)
	if got := e.state(t, other); got != incident.AwaitingApproval {
		t.Fatalf("other incident = %s, want its fix awaiting approval", got)
	}

	e.restore(t, fix)
	if got := e.state(t, id); got != incident.Escalated || e.lastReason(t, id) != "Another fix is already in progress on this workload, so Themis left its change in place." {
		t.Errorf("state = %s: %q", got, e.lastReason(t, id))
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1`, governance.ExecuteArgs{}.Kind()); n != 0 {
		t.Errorf("%d execute jobs, want none", n)
	}
}

// A workload a person already put back needs nothing more.
func TestRestore_AlreadyBack(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Annotations["deployment.kubernetes.io/revision"] = "2"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	e.restore(t, fix)
	if got := e.state(t, id); got != incident.Escalated || e.lastReason(t, id) != "The workload is already back as it was before Themis acted." {
		t.Errorf("state = %s: %q", got, e.lastReason(t, id))
	}
}

// Someone deployed after the verdict: rolling back now would undo their
// change, with nobody asked. Themis leaves it alone and hands over.
func TestRestore_LeavesAChangeMadeAfterTheFix(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.Containers[0].Image = "example/checkout:hotfix"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	e.restore(t, fix)

	if n := len(e.actions(t, id)); n != 1 {
		t.Errorf("%d actions, want the fix alone", n)
	}
	if got := e.state(t, id); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
	if got := e.lastReason(t, id); !strings.Contains(got, "changed after Themis's fix") {
		t.Errorf("reason = %q", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1`, governance.ExecuteArgs{}.Kind()); n != 0 {
		t.Errorf("%d execute jobs, want none", n)
	}
}

// A change that doesn't touch the template isn't someone else's fix: scaling
// is no reason to leave a fix that didn't help in place.
func TestRestore_ScalingDoesNotStopIt(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	replicas := int32(5)
	d.Spec.Replicas = &replicas
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	e.restore(t, fix)
	if n := len(e.actions(t, id)); n != 2 {
		t.Errorf("%d actions, want the fix and its restore", n)
	}
}

func restoreJob(attempt int, actionID string) *river.Job[governance.RestoreArgs] {
	return &river.Job[governance.RestoreArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: 25},
		Args:   governance.RestoreArgs{ActionID: actionID},
	}
}

// A restore that keeps failing retries, and on its last attempt hands the
// incident to a person: never left in restoring with nobody told.
func TestRestoreWorker_EscalatesOnTheLastAttempt(t *testing.T) {
	e := shipped(t)
	id, fix := e.restoring(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET pre_state = '[]' WHERE id = $1`, fix.ID); err != nil {
		t.Fatal(err)
	}
	w := &governance.RestoreWorker{Governor: e.worker}

	if err := w.Work(t.Context(), restoreJob(1, fix.ID)); err == nil {
		t.Fatal("an early attempt reported no error, so it won't be retried")
	}
	if got := e.state(t, id); got != incident.Restoring {
		t.Fatalf("state = %s, want restoring while it retries", got)
	}

	if err := w.Work(t.Context(), restoreJob(25, fix.ID)); err != nil {
		t.Fatalf("last attempt: %v", err)
	}
	if got := e.state(t, id); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
	if got := e.lastReason(t, id); !strings.Contains(got, "couldn't put the workload back") || !strings.Contains(got, "still in place") {
		t.Errorf("reason = %q", got)
	}
}

// When Themis no longer knows how to put a fix back, it leaves the fix in
// place and tells the person it hands the incident to why.
func TestRestore_CannotPutItBack(t *testing.T) {
	for name, tc := range map[string]struct {
		forget func(t *testing.T, e *env)
		want   string
	}{
		"profile removed": {func(_ *testing.T, e *env) { e.worker.Config = config.Config{} }, "The rollout-regression profile was removed from the configuration, so Themis left its change in place."},
		"fix unknown":     {func(_ *testing.T, e *env) { e.worker.Fixes = nil }, "Themis doesn't know how to put this fix back, so it left its change in place."},
		"version before gone": {func(t *testing.T, e *env) {
			rs := &appsv1.ReplicaSet{}
			if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout-7d9f8b6c4"}, rs); err != nil {
				t.Fatal(err)
			}
			if err := k8s.Delete(t.Context(), rs); err != nil {
				t.Fatal(err)
			}
		}, "Themis couldn't put the workload back: Kubernetes no longer keeps the version from before it acted."},
	} {
		e := shipped(t)
		id, fix := e.restoring(t)
		tc.forget(t, e)
		e.restore(t, fix)
		if got := e.state(t, id); got != incident.Escalated || e.lastReason(t, id) != tc.want {
			t.Errorf("%s: %s, %q; want escalated, saying %q", name, got, e.lastReason(t, id), tc.want)
		}
		if n := len(e.actions(t, id)); n != 1 {
			t.Errorf("%s: %d actions, want only the fix", name, n)
		}
	}
}
