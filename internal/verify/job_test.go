package verify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
	"github.com/hzerrad/themis/internal/verify"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

var windowStart = time.Date(2026, 10, 27, 9, 40, 0, 0, time.UTC)

type env struct {
	store  *store.Store
	worker *verify.Worker
	clock  time.Time
	ns     string
	args   verify.VerifyArgs
	inc    string
}

// executed sets up scenario 1 after a rollback was executed, with the
// incident verifying.
func executed(t *testing.T) *env {
	t.Helper()
	ctx := t.Context()
	s, _ := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	d := deploymentIn(t, ns)
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}
	e := &env{store: s, ns: ns, clock: windowStart}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		id, _, err := s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "rollout-regression", Target: target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		e.inc = id
		path := []incident.State{incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval, incident.Authorized, incident.Executing, incident.Verifying}
		for i := 1; i < len(path); i++ {
			if err := s.Transition(ctx, tx, store.TransitionArgs{ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		p, _, err := s.InsertProposal(ctx, tx, store.Proposal{IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p", Capability: "c", CapabilityVersion: "1", Target: target, Params: map[string]any{}, Status: store.ProposalAccepted})
		if err != nil {
			return err
		}
		a := store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Target: digest.TargetIdentity{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: target.UID, Generation: d.Generation},
			Params: map[string]any{"revision": int64(1), "templateHash": restoredHash(t, ns)}, BlastRadius: map[string]any{}, Constraints: map[string]any{}, State: store.ActionSucceeded, Digest: "sha256:d",
		}
		a.ID, err = s.InsertAction(ctx, tx, a)
		if err != nil {
			return err
		}
		e.args = verify.VerifyArgs{ActionID: a.ID, WindowStart: windowStart}
		return s.RecordExecution(ctx, tx, store.Execution{ActionID: a.ID, Status: "succeeded", PostGeneration: d.Generation, StartedAt: windowStart, FinishedAt: windowStart})
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{
		Name: "rollout-regression", Evidence: config.Evidence{Allowed: []string{evidence.PodStates}},
		Verification: config.Verification{Window: config.Duration{Duration: 5 * time.Minute}, Stability: config.Duration{Duration: 2 * time.Minute}},
	}
	e.worker = &verify.Worker{Config: config.Config{Profiles: []config.Profile{profile}}, Store: s, Reader: k8s, Now: func() time.Time { return e.clock }}
	return e
}

func deploymentIn(t *testing.T, ns string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// restored is revision 1's template, as the rollback applies it.
func restored(t *testing.T, ns string) corev1.PodTemplateSpec {
	t.Helper()
	var rs appsv1.ReplicaSet
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout-5c6a1e2f3"}, &rs); err != nil {
		t.Fatal(err)
	}
	tmpl := *rs.Spec.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return tmpl
}

func restoredHash(t *testing.T, ns string) string {
	t.Helper()
	h, err := rollback.TemplateHash(restored(t, ns))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// rolledBack applies the rollback and completes it, as the Deployment
// controller would: revision 1 becomes revision 3.
func (e *env) rolledBack(t *testing.T) {
	t.Helper()
	d := deploymentIn(t, e.ns)
	d.Spec.Template = restored(t, e.ns)
	d.Annotations["deployment.kubernetes.io/revision"] = "3"
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	var rs appsv1.ReplicaSet
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.ns, Name: "checkout-5c6a1e2f3"}, &rs); err != nil {
		t.Fatal(err)
	}
	rs.Annotations["deployment.kubernetes.io/revision"] = "3"
	if err := k8s.Update(t.Context(), &rs); err != nil {
		t.Fatal(err)
	}
	d = deploymentIn(t, e.ns)
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UnavailableReplicas = 3, 3, 3, 3, 0
	if err := k8s.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

func (e *env) verify(t *testing.T) bool {
	t.Helper()
	final, err := e.worker.Verify(t.Context(), e.args)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return final
}

func (e *env) state(t *testing.T) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), e.inc)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestJob_StableRollback_Resolves(t *testing.T) {
	e := executed(t)
	e.rolledBack(t)
	if e.verify(t) {
		t.Fatal("final before the target was stable for long enough")
	}
	e.clock = windowStart.Add(3 * time.Minute)
	if !e.verify(t) {
		t.Fatal("not final after the stability period")
	}
	if got := e.state(t); got != incident.Resolved {
		t.Errorf("state = %s, want resolved", got)
	}
}

// applied applies the rollback, but the rollout never completes.
func (e *env) applied(t *testing.T) {
	t.Helper()
	d := deploymentIn(t, e.ns)
	d.Spec.Template = restored(t, e.ns)
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

func (e *env) outcome(t *testing.T) string {
	t.Helper()
	v, err := e.store.Verification(t.Context(), e.store.DB(), e.args.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	return v.Outcome
}

func TestJob_FailedOutcome_Escalates(t *testing.T) {
	e := executed(t)
	e.applied(t)
	e.verify(t)
	e.clock = windowStart.Add(6 * time.Minute)
	if !e.verify(t) {
		t.Fatal("not final after the window")
	}
	if got := e.state(t); got != incident.Escalated || e.outcome(t) != string(verify.Unchanged) {
		t.Errorf("state = %s after %s, want escalated after unchanged", got, e.outcome(t))
	}
}

// Review Focus 3: the window is measured from the persisted start, not from
// whenever the worker came back.
func TestJob_ResumesAfterRestart_UsesPersistedWindowStart(t *testing.T) {
	e := executed(t)
	e.applied(t)
	e.verify(t)
	restarted := &verify.Worker{Config: e.worker.Config, Store: e.store, Reader: k8s, Now: func() time.Time { return windowStart.Add(5*time.Minute + time.Second) }}
	final, err := restarted.Verify(t.Context(), e.args)
	if err != nil || !final || e.outcome(t) != string(verify.Unchanged) {
		t.Fatalf("Verify after restart = %v, %v, %s; want unchanged at the original window end", final, err, e.outcome(t))
	}
}

func TestJob_HumanEditDuringWindow_Inconclusive(t *testing.T) {
	e := executed(t)
	e.rolledBack(t)
	d := deploymentIn(t, e.ns)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if !e.verify(t) {
		t.Fatal("contamination was not final")
	}
	if got := e.outcome(t); got != string(verify.Inconclusive) {
		t.Errorf("outcome = %q, want inconclusive", got)
	}
	trail, err := e.store.Transitions(t.Context(), e.inc)
	if err != nil || !strings.Contains(trail[len(trail)-1].Reason, "changed outside Themis") {
		t.Errorf("the escalation does not say why: %+v", trail[len(trail)-1])
	}
	if got := e.state(t); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}

// failingReader stands in for an API server that refuses every read.
type failingReader struct{}

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("forbidden")
}

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("forbidden")
}

func TestJob_UnobservableOnTheLastAttempt_Escalates(t *testing.T) {
	e := executed(t)
	w := &verify.Worker{Config: e.worker.Config, Store: e.store, Reader: failingReader{}, Now: func() time.Time { return windowStart }}
	job := &river.Job[verify.VerifyArgs]{JobRow: &rivertype.JobRow{Attempt: 10, MaxAttempts: 10}, Args: e.args}
	if err := w.Work(t.Context(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := e.state(t); got != incident.Escalated {
		t.Errorf("state = %s, want escalated", got)
	}
}
