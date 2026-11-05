package governance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
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
	target := cluster(t)
	cfg := config.Config{EnrolledNamespaces: []string{target.Namespace}, Profiles: []config.Profile{profile("rollout-regression"), profile("other")}}
	w := &governance.Worker{Config: cfg, Store: s, Reader: k8s, Fixes: catalog.Fixes()}
	w.Now = func() time.Time { return now }
	return &env{worker: w, store: s, pool: pool, target: target}
}

// shipped governs with the policy of the rollout-regression scenario pack.
func shipped(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	p := profile("rollout-regression")
	p.Policy = shippedProfile(t).Policy
	e.worker.Config = config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}
	return e
}

var predicates = []check.Result{
	{Name: "oom_killed_new_revision", Version: "1", Value: check.True, Reason: "x"},
	{Name: governance.ManagedCheck, Version: "1", Value: check.False, Reason: "x"},
}

// diagnosed creates an incident diagnosed with an accepted proposal to roll
// back to revision, and returns the proposal's ID.
func (e *env) diagnosed(t *testing.T, profile string, revision int64, results []check.Result) (string, string) {
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
		if err := e.store.TransitionPath(ctx, tx, incidentID, "test", "test", incident.Received, incident.Investigating, incident.Diagnosed); err != nil {
			return err
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

// govern runs the govern job once, as River would.
func (e *env) govern(t *testing.T, proposalID string) {
	t.Helper()
	job := &river.Job[governance.GovernArgs]{Args: governance.GovernArgs{ProposalID: proposalID}}
	if err := e.worker.Work(t.Context(), job); err != nil {
		t.Fatalf("Work: %v", err)
	}
}

func decisions(t *testing.T, decision string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_policy_decisions_total", map[string]string{"decision": decision})
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

// A fix policy doesn't forbid waits for a person's approval, until the
// profile's approval timeout.
func TestGovern_AwaitsApproval(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)

	if got := e.state(t, id); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
	a := e.action(t, id)
	if a.State != store.ActionAwaitingApproval || a.Decision != "require_approval" || a.ProposalID != p || a.ExpiresAt == nil || !a.ExpiresAt.Equal(now.Add(config.DefaultApprovalTimeout)) {
		t.Errorf("action = %s, %s, for %s, expiring %v", a.State, a.Decision, a.ProposalID, a.ExpiresAt)
	}
}

// A fix policy forbids closes the incident without acting, and a denial is
// counted as a GitOps refusal only when GitOps owns the target.
func TestGovern_Denied(t *testing.T) {
	for _, tt := range []struct {
		name     string
		checks   []check.Result
		gitOps   bool
		refusals float64
	}{
		{"GitOps owns the target", predicates, true, 1},
		{"forbidden by a rule", []check.Result{{Name: "oom_killed_new_revision", Version: "1", Value: check.False, Reason: "x"}, predicates[1]}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.gitOps {
				argoManaged(t, e.target)
			}
			deny := decisions(t, "deny")
			refusals := metrictest.Delta(t, "themis_gitops_refusals_total", nil)
			id, p := e.diagnosed(t, "rollout-regression", 1, tt.checks)
			e.govern(t, p)

			if a := e.action(t, id); a.State != store.ActionDenied || a.ExpiresAt != nil {
				t.Errorf("action = %s, want denied", a.State)
			}
			if got := e.state(t, id); got != incident.ClosedNoAction {
				t.Errorf("state = %s, want closed_no_action", got)
			}
			if got := deny(); got != 1 {
				t.Errorf("counted %v denials, want 1", got)
			}
			if got := refusals(); got != tt.refusals {
				t.Errorf("counted %v GitOps refusals, want %v", got, tt.refusals)
			}
		})
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

// A repeated govern job decides once.
func TestGovern_Idempotent(t *testing.T) {
	e := newEnv(t)
	approval := decisions(t, "require_approval")
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	e.govern(t, p)
	e.action(t, id)
	if got := approval(); got != 1 {
		t.Errorf("counted %v require_approval decisions, want 1", got)
	}
}

// A suggestion Themis can't turn into a fix closes the incident without one.
func TestGovern_ClosesWithoutAction(t *testing.T) {
	tests := map[string]struct {
		profile  string
		revision int64
		mutate   func(t *testing.T, e *env, proposalID string)
	}{
		"revision missing": {profile: "rollout-regression", revision: 9},
		"invalid revision": {profile: "rollout-regression", revision: 0},
		"profile removed":  {profile: "removed", revision: 1},
		"unknown fix": {profile: "rollout-regression", revision: 1, mutate: func(t *testing.T, e *env, proposalID string) {
			if _, err := e.pool.Exec(t.Context(), `UPDATE proposals SET capability = 'deployment.teleport' WHERE id = $1`, proposalID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			id, p := e.diagnosed(t, tt.profile, tt.revision, predicates)
			if tt.mutate != nil {
				tt.mutate(t, e, p)
			}
			e.govern(t, p)
			if got := e.state(t, id); got != incident.ClosedNoAction {
				t.Errorf("state = %s, want closed_no_action", got)
			}
			if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1`, id); n != 0 {
				t.Errorf("%d actions, want none", n)
			}
		})
	}
}

// A rule that can't be evaluated forbids the fix, and is counted as a failed
// rule, not as a denial a rule decided.
func TestGovern_RuleThatFailsForbids(t *testing.T) {
	e := newEnv(t)
	p := profile("rollout-regression")
	p.Policy.Rules = append([]config.PolicyRule{{Name: "typo", When: `predicates.no_such_check == "true"`, Decision: config.DecisionRequireApproval}}, p.Policy.Rules...)
	e.worker.Config = config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}
	failed, deny := decisions(t, "error"), decisions(t, "deny")
	id, proposal := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, proposal)

	if a := e.action(t, id); a.State != store.ActionDenied || a.PolicyID != "typo" || !strings.Contains(a.DecisionReason, "couldn't be evaluated") {
		t.Errorf("action = %s by %s: %q, want denied by typo, which couldn't be evaluated", a.State, a.PolicyID, a.DecisionReason)
	}
	if got := e.state(t, id); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
	if f, d := failed(), deny(); f != 1 || d != 0 {
		t.Errorf("counted %v failed rules and %v denials, want 1 and 0", f, d)
	}
}

// unreachable stands in for an API server Themis can't reach.
type unreachable struct{ client.Reader }

func (unreachable) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("the API server is unreachable")
}

func (unreachable) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the API server is unreachable")
}

// When the cluster can't be read, Themis decides nothing: the job fails, to
// be retried, and the incident waits where it was.
func TestGovern_UnreadableClusterIsRetried(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.worker.Reader = unreachable{}
	if err := e.worker.Govern(t.Context(), p); err == nil {
		t.Error("Govern = nil, want an error so the job is retried")
	}
	if got := e.state(t, id); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1`, id); n != 0 {
		t.Errorf("%d actions, want none", n)
	}
}

// Job kinds are stored with queued jobs: renaming one strands every job of
// the old name across an upgrade.
func TestJobKinds(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{governance.GovernArgs{}.Kind(), "govern"},
		{governance.ExecuteArgs{}.Kind(), "execute"},
		{governance.RestoreArgs{}.Kind(), "restore"},
	} {
		if tt.got != tt.want {
			t.Errorf("kind %q, want %q", tt.got, tt.want)
		}
	}
}
