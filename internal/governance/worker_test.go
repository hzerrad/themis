package governance_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const scenario1 = "test/fixtures/scenario1/cluster.yaml"

var now = time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)

func profile(name string) config.Profile {
	return config.Profile{
		Name: name, Priority: 100,
		Evidence:     config.Evidence{Allowed: []string{evidence.PodStates}},
		Capabilities: []string{"deployment.rollback-to-revision"},
		Policy: config.Policy{Mode: config.ModeGuarded, Rules: []config.PolicyRule{
			{Name: "gitops", When: `predicates.gitops_managed != "false"`, Decision: config.DecisionDeny},
			{Name: "needs-oom", When: `predicates.oom_killed_new_revision != "true"`, Decision: config.DecisionDeny},
			{Name: "approve", When: "true", Decision: config.DecisionRequireApproval},
		}},
	}
}

type env struct {
	worker *governance.Worker
	store  *store.Store
	pool   *pgxpool.Pool
	target incident.TargetRef
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, pool := pgtest.NewStore(t)
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{EnrolledNamespaces: []string{ns}, Profiles: []config.Profile{profile("rollout-regression"), profile("other")}}
	w, err := governance.NewWorker(cfg, s, k8s)
	if err != nil {
		t.Fatal(err)
	}
	w.Now = func() time.Time { return now }
	return &env{worker: w, store: s, pool: pool, target: incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}}
}

var predicates = []predicate.Result{
	{Name: predicate.NameOOMKilledNewRevision, Version: "1", Value: predicate.True, Reason: "x"},
	{Name: predicate.NameGitOpsManaged, Version: "1", Value: predicate.False, Reason: "x"},
}

// diagnosed creates an incident diagnosed with an accepted proposal to roll
// back to revision, and returns the proposal's ID.
func (e *env) diagnosed(t *testing.T, profile string, revision int64, results []predicate.Result) (string, string) {
	t.Helper()
	ctx := t.Context()
	var incidentID, proposalID string
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		incidentID, _, err = e.store.CreateOrJoinIncident(ctx, tx, store.IncidentArgs{
			Fingerprint: profile + "/" + e.target.UID, Profile: profile, Target: e.target, Payload: []byte(`{}`),
		})
		if err != nil {
			return err
		}
		for _, step := range [][2]incident.State{{incident.Received, incident.Investigating}, {incident.Investigating, incident.Diagnosed}} {
			if err := e.store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: step[0], To: step[1], Reason: "test", Actor: "test"}); err != nil {
				return err
			}
		}
		_, err = e.store.CreateInvestigation(ctx, tx, incidentID, nil, results)
		if err != nil {
			return err
		}
		p, _, err := e.store.InsertProposal(ctx, tx, store.Proposal{
			IncidentID: incidentID, LeaseID: "0192f0c1-0000-7000-8000-" + strings.Repeat("0", 11) + "1", ClientProposalID: incidentID,
			Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Target: e.target, Params: map[string]any{"revision": revision}, Status: store.ProposalAccepted,
		})
		proposalID = p.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return incidentID, proposalID
}

func (e *env) govern(t *testing.T, proposalID string) {
	t.Helper()
	if err := e.worker.Govern(t.Context(), proposalID); err != nil {
		t.Fatalf("Govern: %v", err)
	}
}

func (e *env) state(t *testing.T, id string) incident.State {
	t.Helper()
	in, err := e.store.Incident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return in.State
}

func (e *env) action(t *testing.T, incidentID string) store.Action {
	t.Helper()
	actions, err := e.store.IncidentActions(t.Context(), e.store.DB(), incidentID)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %v, %v; want one", actions, err)
	}
	return actions[0]
}

func TestGovern_RequiresApprovalInGuarded(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)

	if got := e.state(t, id); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
	a := e.action(t, id)
	if a.State != store.ActionAwaitingApproval || a.Digest == "" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(now.Add(config.DefaultApprovalTimeout)) {
		t.Errorf("action = %+v", a)
	}
	if a.PolicyID != "approve" || a.Target.UID != e.target.UID || a.Target.SpecHash == "" {
		t.Errorf("action identity or decision = %+v", a)
	}
}

// The action comes from the cluster, not from what the proposal says.
func TestDerive_ComputesFromLiveStateNotProposal(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	a := e.action(t, id)
	if a.BlastRadius["replicas"] != int64(3) {
		t.Errorf("blast radius = %v, want the live 3 replicas", a.BlastRadius)
	}
	if a.Params["templateHash"] == "" || a.Params["revision"] != int64(1) {
		t.Errorf("params = %v", a.Params)
	}
}

func TestDerive_CapturesPreStateTemplate(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	a := e.action(t, id)
	var pre, desired appsv1.Deployment
	if err := json.Unmarshal(a.PreState, &pre.Spec.Template); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(a.Desired, &desired.Spec.Template); err != nil {
		t.Fatal(err)
	}
	if pre.Spec.Template.Spec.Containers[0].Image != "example/checkout:2.4.0" || desired.Spec.Template.Spec.Containers[0].Image != "example/checkout:2.3.1" {
		t.Errorf("pre-state %s, desired %s", pre.Spec.Template.Spec.Containers[0].Image, desired.Spec.Template.Spec.Containers[0].Image)
	}
}

// GitOps ownership is read live: a stale predicate does not hide it.
func TestOwnership_ArgoManaged_Denied(t *testing.T) {
	e := newEnv(t)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Annotations["argocd.argoproj.io/tracking-id"] = "shop:apps/Deployment:shop/checkout"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)

	if a := e.action(t, id); a.State != store.ActionDenied || a.PolicyID != "gitops" {
		t.Errorf("action = %s by %s, want denied by gitops", a.State, a.PolicyID)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
}

var unknownPredicates = []predicate.Result{{Name: predicate.NameGitOpsManaged, Version: "1", Value: predicate.False, Reason: "x"}, {Name: predicate.NameOOMKilledNewRevision, Version: "1", Value: predicate.Unknown, Reason: "x"}}

func TestPolicy_UnknownPredicateDenies(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, unknownPredicates)
	e.govern(t, p)
	if a := e.action(t, id); a.State != store.ActionDenied {
		t.Errorf("action = %s, want denied", a.State)
	}
}

func TestOneActiveActionPerTarget(t *testing.T) {
	e := newEnv(t)
	_, first := e.diagnosed(t, "rollout-regression", 1, predicates)
	second, p := e.diagnosed(t, "other", 1, predicates)
	e.govern(t, first)
	e.govern(t, p)

	proposal, err := e.store.Proposal(t.Context(), e.store.DB(), p)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Status != store.ProposalDeferred {
		t.Errorf("second proposal = %s, want deferred", proposal.Status)
	}
	if got := e.state(t, second); got != incident.ClosedNoAction {
		t.Errorf("second incident = %s, want closed_no_action", got)
	}
}

func TestGovern_Idempotent(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	e.govern(t, p)
	e.action(t, id)
}

func TestGovern_RevisionMissing_ClosesWithoutAction(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 9, predicates)
	e.govern(t, p)
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1`, id); n != 0 {
		t.Errorf("%d actions, want none", n)
	}
}
