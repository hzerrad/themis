package reconciliation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/incident/reconciliation"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
	"github.com/hzerrad/themis/internal/workload"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

type env struct {
	store  *store.Store
	pool   *pgxpool.Pool
	worker *reconciliation.Worker
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
	d := k8s.Deployment(t, ns, "checkout")
	return &env{
		store: s, pool: pool, ns: ns,
		worker: &reconciliation.Worker{Store: s, Reader: k8s, Now: later},
		target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)},
	}
}

// open creates an incident in state with the target's current template
// recorded, and an action when the state has one.
func (e *env) open(t *testing.T, to incident.State) string {
	t.Helper()
	ctx := t.Context()
	snap, err := workload.Read(ctx, k8s, e.target)
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
		if err := e.store.SetObservedSpec(ctx, tx, id, snap.TemplateHash); err != nil {
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
	d := k8s.Deployment(t, e.ns, "checkout")
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

// heal completes the rollout with every replica ready.
func (e *env) heal(t *testing.T) {
	t.Helper()
	d := k8s.Deployment(t, e.ns, "checkout")
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

// An incident handed to a person, waiting for the fix they suggest, is
// watched too: fixed by hand, it closes as recovered; changed and still
// failing, a new incident replaces it, so the next alert isn't lost in it.
func TestReconcile_WaitingForAPersonsFix(t *testing.T) {
	for name, fixed := range map[string]bool{"fixed by hand": true, "changed and still failing": false} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id := e.open(t, incident.Investigating)
			err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
				return e.store.Transition(t.Context(), tx, store.TransitionArgs{
					ID: id, From: incident.Investigating, To: incident.AwaitingSuggestion,
					Reason: "No fix proposed. Referred to a person.", Actor: "test", Refer: true,
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			e.edit(t)
			want := incident.Canceled
			if fixed {
				e.heal(t)
				want = incident.SelfResolved
			}
			e.reconcile(t)
			if got := e.incident(t, id).State; got != want {
				t.Errorf("state = %s, want %s", got, want)
			}
		})
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
	if err := k8s.Delete(t.Context(), k8s.Deployment(t, e.ns, "checkout")); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if got := e.incident(t, id).State; got != incident.Canceled {
		t.Errorf("state = %s, want canceled", got)
	}
}

// unreadable fails every read in one namespace, as when the API server
// errors there.
type unreadable struct {
	client.Reader
	ns string
}

func (r unreadable) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Namespace == r.ns {
		return errors.New("the server is currently unable to handle the request")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// A target that can't be read leaves its incident as it was, and the others
// are still reconciled.
func TestReconcile_UnreadableTargetDoesNotStopTheOthers(t *testing.T) {
	e := newEnv(t)
	fixed := e.open(t, incident.AwaitingApproval)
	e.edit(t)
	e.heal(t)

	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	other := &env{
		store: e.store, pool: e.pool, ns: ns,
		target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(k8s.Deployment(t, ns, "checkout").UID)},
	}
	stuck := other.open(t, incident.AwaitingApproval)
	other.edit(t)
	other.heal(t)

	e.worker.Reader = unreadable{Reader: k8s, ns: ns}
	e.reconcile(t)
	if got := e.incident(t, stuck).State; got != incident.AwaitingApproval {
		t.Errorf("unreadable target's incident = %s, want awaiting_approval", got)
	}
	if got := e.incident(t, fixed).State; got != incident.SelfResolved {
		t.Errorf("readable target's incident = %s, want self_resolved", got)
	}
}
