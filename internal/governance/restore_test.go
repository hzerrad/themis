package governance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// restoring governs a rollback to revision 1, records it as applied without
// helping, and leaves the incident restoring. It returns the incident and the
// fix.
func (e *env) restoring(t *testing.T) (string, store.Action) {
	t.Helper()
	id, p := e.diagnosed(t, "rollout-regression", 1, facts(predicate.True))
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
	if r.State != store.ActionAuthorized || r.Decision != string(governance.Allow) || r.ExpiresAt != nil {
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

// When policy asks a person to approve putting things back, Themis leaves
// its change in place and hands the incident over.
func TestRestore_PolicyAskingAPersonEscalates(t *testing.T) {
	e := newEnv(t)
	p := profile("rollout-regression")
	p.Policy.Rules = append([]config.PolicyRule{{Name: "ask-first", When: `action.restore == "true"`, Decision: config.DecisionRequireApproval}}, p.Policy.Rules...)
	w, err := governance.NewWorker(config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}, e.store, k8s)
	if err != nil {
		t.Fatal(err)
	}
	w.Now = e.worker.Now
	id, fix := e.restoring(t)
	e.worker = w
	e.restore(t, fix)

	if got := e.state(t, id); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
	if got := e.lastReason(t, id); !strings.Contains(got, "Policy rule ask-first requires a person's approval.") || !strings.Contains(got, "left its change in place") {
		t.Errorf("reason = %q", got)
	}
	if n := len(e.actions(t, id)); n != 1 {
		t.Errorf("%d actions, want only the fix", n)
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
