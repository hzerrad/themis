package executor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/executor"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
	"github.com/hzerrad/themis/internal/verify"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

var now = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

type env struct {
	store  *store.Store
	pool   *pgxpool.Pool
	target incident.TargetRef
}

func profile() config.Profile {
	return config.Profile{
		Name: "rollout-regression", Priority: 100,
		Evidence:     config.Evidence{Allowed: []string{evidence.PodStates}},
		Capabilities: []string{"deployment.rollback-to-revision"},
	}
}

// approved runs the scenario 1 incident through governance and approval, and
// returns the authorized action.
func approved(t *testing.T) (*env, store.Action) {
	t.Helper()
	ctx := t.Context()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	d := deployment(t, ns)
	e := &env{store: s, pool: pool, target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}}

	var incidentID, proposalID string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		incidentID, _, err = s.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{Fingerprint: "fp", Profile: "rollout-regression", Target: e.target, Payload: []byte(`{}`)})
		if err != nil {
			return err
		}
		for _, step := range [][2]incident.State{{incident.Received, incident.Investigating}, {incident.Investigating, incident.Diagnosed}} {
			if err := s.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: step[0], To: step[1], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		if _, err := s.CreateInvestigation(ctx, tx, incidentID, nil, []predicate.Result{{Name: predicate.NameGitOpsManaged, Version: "1", Value: predicate.False}}); err != nil {
			return err
		}
		p, _, err := s.InsertProposal(ctx, tx, store.Proposal{
			IncidentID: incidentID, LeaseID: "0192f0c1-0000-7000-8000-000000000001", ClientProposalID: "p",
			Capability: "deployment.rollback-to-revision", CapabilityVersion: "1", Target: e.target,
			Params: map[string]any{"revision": float64(1)}, Status: store.ProposalAccepted,
		})
		proposalID = p.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	gov, err := governance.NewWorker(config.Config{Profiles: []config.Profile{profile()}}, s, k8s)
	if err != nil {
		t.Fatal(err)
	}
	gov.Now = func() time.Time { return now }
	if err := gov.Govern(ctx, proposalID); err != nil {
		t.Fatalf("Govern: %v", err)
	}
	actions, err := s.IncidentActions(ctx, s.DB(), incidentID)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v", actions, err)
	}
	a := actions[0]
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Approve(ctx, tx, a.ID, a.Digest, "admin", now); err != nil {
			return err
		}
		return s.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: incident.AwaitingApproval, To: incident.Authorized, Reason: "test", Actor: "test"})
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return e, a
}

func deployment(t *testing.T, ns string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func (e *env) executor(c client.Client) *executor.Executor {
	return &executor.Executor{Store: e.store, Client: c, Now: func() time.Time { return now.Add(time.Minute) }}
}

func (e *env) image(t *testing.T) string {
	t.Helper()
	return deployment(t, e.target.Namespace).Spec.Template.Spec.Containers[0].Image
}

func (e *env) actionState(t *testing.T, id string) store.ActionState {
	t.Helper()
	a, err := e.store.Action(t.Context(), e.store.DB(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.State
}

func (e *env) incidentState(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func TestExecute_RestoresPriorTemplate(t *testing.T) {
	e, a := approved(t)
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != executor.Succeeded || res.PostGeneration == 0 {
		t.Errorf("result = %+v", res)
	}
	if got := e.image(t); got != "example/checkout:2.3.1" {
		t.Errorf("image = %s, want revision 1's", got)
	}
	if got := e.actionState(t, a.ID); got != store.ActionSucceeded {
		t.Errorf("action = %s, want succeeded", got)
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Verifying {
		t.Errorf("incident = %s, want verifying", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'actionID' = $2 AND args->>'incidentID' = $3`, verify.VerifyArgs{}.Kind(), a.ID, a.IncidentID); n != 1 {
		t.Errorf("%d verify jobs for the action and its incident, want 1", n)
	}
}

// A second delivery of the same execution writes nothing more.
func TestExecute_Twice_WritesOnce(t *testing.T) {
	e, a := approved(t)
	x := e.executor(k8s.Client)
	first, err := x.Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen := deployment(t, e.target.Namespace).Generation
	second, err := x.Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if second != first || deployment(t, e.target.Namespace).Generation != gen {
		t.Errorf("second execution = %+v, generation moved from %d", second, gen)
	}
}

func TestExecute_DigestTampered_Refuses(t *testing.T) {
	e, a := approved(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET params = jsonb_set(params, '{revision}', '2') WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Stale || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s; want stale and untouched", res, e.image(t))
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("incident = %s, want escalated", got)
	}
}

func TestExecute_SpecChangedSinceApproval_Stale(t *testing.T) {
	e, a := approved(t)
	d := deployment(t, e.target.Namespace)
	d.Spec.Template.Annotations = map[string]string{"edited": "by-a-human"}
	if err := k8s.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Stale || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s", res, e.image(t))
	}
}

// resourceVersion is not bound: a status update after approval is fine.
func TestExecute_StatusOnlyChange_StillExecutes(t *testing.T) {
	e, a := approved(t)
	d := deployment(t, e.target.Namespace)
	d.Status.ReadyReplicas, d.Status.AvailableReplicas = 2, 2
	if err := k8s.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID); err != nil || res.Status != executor.Succeeded {
		t.Errorf("Execute = %+v, %v; want succeeded", res, err)
	}
}

func TestExecute_NotAuthorized(t *testing.T) {
	e, a := approved(t)
	if _, err := e.pool.Exec(t.Context(), `UPDATE actions SET state = 'awaiting_approval' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executor(k8s.Client).Execute(t.Context(), a.ID); !errors.Is(err, store.ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized", err)
	}
}

// failing fails dry-run patches, or real ones, with err.
func failing(t *testing.T, dryRun bool, err error) client.Client {
	t.Helper()
	c, newErr := client.NewWithWatch(k8s.Config, client.Options{})
	if newErr != nil {
		t.Fatal(newErr)
	}
	return interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			po := &client.PatchOptions{}
			po.ApplyOptions(opts)
			if (len(po.DryRun) > 0) == dryRun {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

func TestExecute_DryRunFailure_NoWrite(t *testing.T) {
	e, a := approved(t)
	res, err := e.executor(failing(t, true, errors.New("admission webhook denied the request"))).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Failed || e.image(t) != "example/checkout:2.4.0" {
		t.Errorf("result = %+v, image = %s", res, e.image(t))
	}
}

// After a request is sent, a timeout leaves the write unknown: it is never
// retried blindly.
func TestExecute_TimeoutAfterSend_Ambiguous(t *testing.T) {
	e, a := approved(t)
	res, err := e.executor(failing(t, false, context.DeadlineExceeded)).Execute(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != executor.Ambiguous {
		t.Errorf("result = %+v, want ambiguous", res)
	}
	if got := e.actionState(t, a.ID); got != store.ActionAmbiguous {
		t.Errorf("action = %s, want ambiguous", got)
	}
}

func TestExecute_InterruptedClaim_Ambiguous(t *testing.T) {
	e, a := approved(t)
	err := e.store.InTx(t.Context(), func(tx pgx.Tx) error {
		locked, err := e.store.Action(t.Context(), tx, a.ID)
		if err != nil {
			return err
		}
		if err := e.store.ClaimExecution(t.Context(), tx, locked, now); err != nil {
			return err
		}
		return e.store.Transition(t.Context(), tx, store.TransitionArgs{ID: a.IncidentID, From: incident.Authorized, To: incident.Executing, Reason: "test", Actor: "test"})
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.executor(k8s.Client).Execute(t.Context(), a.ID)
	if err != nil || res.Status != executor.Ambiguous {
		t.Errorf("Execute = %+v, %v; want ambiguous", res, err)
	}
	if got := e.incidentState(t, a.IncidentID); got != incident.Escalated {
		t.Errorf("incident = %s, want escalated", got)
	}
}
