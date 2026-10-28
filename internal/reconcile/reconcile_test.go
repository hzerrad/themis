package reconcile_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/investigation"
	"github.com/hzerrad/themis/internal/reconcile"
	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

type env struct {
	store  *store.Store
	pool   *pgxpool.Pool
	worker *reconcile.Worker
	ns     string
	target incident.TargetRef
}

// later is past the settle time of any change made by a test.
var later = func() time.Time { return time.Now().Add(5 * time.Minute) }

func newEnv(t *testing.T) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	d := deployment(t, ns)
	return &env{
		store: s, pool: pool, ns: ns,
		worker: &reconcile.Worker{Store: s, Reader: k8s, Now: later},
		target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)},
	}
}

func deployment(t *testing.T, ns string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

// open creates an incident in state with the target's current template
// recorded, and an action when the state has one.
func (e *env) open(t *testing.T, to incident.State) string {
	t.Helper()
	ctx := t.Context()
	snap, err := rollback.Snapshot(ctx, k8s, e.target)
	if err != nil {
		t.Fatal(err)
	}
	path := []incident.State{incident.Received, incident.Investigating, incident.Diagnosed, incident.Proposed, incident.AwaitingApproval, incident.Authorized}
	var id string
	err = e.store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		id, _, err = e.store.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "rollout-regression/Deployment.apps/" + e.ns + "/checkout/" + e.target.UID, Profile: "rollout-regression", Target: e.target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		if err := e.store.SetObservedSpec(ctx, tx, id, snap.SpecHash); err != nil {
			return err
		}
		if err := e.store.RecordFiringAlert(ctx, tx, store.FiringAlertArgs{IncidentID: id, Source: "alertmanager", SourceID: "a1", StartsAt: time.Now()}); err != nil {
			return err
		}
		for i := 1; i < len(path) && path[i-1] != to; i++ {
			if err := e.store.Transition(ctx, tx, store.TransitionArgs{ID: id, From: path[i-1], To: path[i], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		if to != incident.AwaitingApproval && to != incident.Authorized {
			return nil
		}
		p, _, err := e.store.InsertProposal(ctx, tx, store.Proposal{IncidentID: id, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p", Capability: "c", CapabilityVersion: "1", Target: e.target, Params: map[string]any{}, Status: store.ProposalAccepted})
		if err != nil {
			return err
		}
		state := store.ActionAwaitingApproval
		if to == incident.Authorized {
			state = store.ActionAuthorized
		}
		_, err = e.store.InsertAction(ctx, tx, store.Action{
			IncidentID: id, ProposalID: p.ID, Capability: "c", Target: digest.TargetIdentity{UID: e.target.UID},
			Params: map[string]any{}, BlastRadius: map[string]any{}, Constraints: map[string]any{}, State: state, Digest: "d",
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// edit changes the template, as kubectl set env would.
func (e *env) edit(t *testing.T) {
	t.Helper()
	d := deployment(t, e.ns)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

// heal completes the rollout with every replica ready.
func (e *env) heal(t *testing.T) {
	t.Helper()
	d := deployment(t, e.ns)
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UnavailableReplicas = 3, 3, 3, 3, 0
	if err := k8s.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

func (e *env) reconcile(t *testing.T) {
	t.Helper()
	if err := e.worker.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func (e *env) incident(t *testing.T, id string) store.Incident {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestReconcile_UnchangedTargetIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	id := e.open(t, incident.AwaitingApproval)
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
}

func TestReconcile_FixedOutsideThemis_SelfResolves(t *testing.T) {
	e := newEnv(t)
	id := e.open(t, incident.AwaitingApproval)
	e.edit(t)
	e.heal(t)
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.SelfResolved {
		t.Errorf("state = %s, want self_resolved", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1 AND state = 'canceled'`, id); n != 1 {
		t.Error("the pending action was not canceled")
	}
	trail, _ := e.store.Transitions(t.Context(), id)
	if r := trail[len(trail)-1].Reason; r == "" || r == "test" {
		t.Errorf("reason = %q, want who fixed it", r)
	}
}

// Authorized cannot self-resolve: the action is in flight.
func TestReconcile_FixedWhileAuthorized_Cancels(t *testing.T) {
	e := newEnv(t)
	id := e.open(t, incident.Authorized)
	e.edit(t)
	e.heal(t)
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.Canceled {
		t.Errorf("state = %s, want canceled", got)
	}
}

func TestReconcile_ChangedButFailing_Supersedes(t *testing.T) {
	e := newEnv(t)
	id := e.open(t, incident.AwaitingApproval)
	e.edit(t)
	e.reconcile(t)

	old := e.incident(t, id)
	if old.State != incident.Canceled || old.SupersededBy == "" {
		t.Fatalf("old incident = %s, superseded by %q", old.State, old.SupersededBy)
	}
	succ := e.incident(t, old.SupersededBy)
	if succ.State != incident.Received || succ.Target != e.target {
		t.Errorf("successor = %+v", succ)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'incidentID' = $2`, investigation.InvestigateArgs{}.Kind(), succ.ID); n != 1 {
		t.Errorf("%d investigations queued for the successor, want 1", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND status = 'firing'`, succ.ID); n != 1 {
		t.Error("the firing alert was not carried over")
	}
}

// A rollout in progress is not judged yet.
func TestReconcile_WaitsForAChangeToSettle(t *testing.T) {
	e := newEnv(t)
	e.worker.Now = time.Now
	id := e.open(t, incident.AwaitingApproval)
	e.edit(t)
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval until the change settles", got)
	}
}

func TestReconcile_TargetGone_Cancels(t *testing.T) {
	e := newEnv(t)
	id := e.open(t, incident.Investigating)
	if err := k8s.Delete(t.Context(), deployment(t, e.ns)); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.Canceled {
		t.Errorf("state = %s, want canceled", got)
	}
}
