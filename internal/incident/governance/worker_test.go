package governance_test

import (
	"context"
	"errors"
	"slices"
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
		JustifiedBy:  map[string][]string{"deployment.rollback-to-revision": {"oom_killed_new_revision"}},
		Policy: config.Policy{Mode: config.ModeGuarded, Rules: []config.PolicyRule{
			{Name: "needs-oom", When: `predicates.oom_killed_new_revision != "true"`, Decision: config.DecisionDeny},
			{Name: "no-instructions", When: `"evidence_has_instructions" in predicates && predicates.evidence_has_instructions == "true"`, Decision: config.DecisionDeny},
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
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &governance.Worker{Config: cfg, Store: s, Reader: k8s, Fixes: catalog.Fixes(), Checks: cat.Checks, Recheck: cat}
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
	{Name: check.GitOpsManaged, Version: "1", Value: check.False, Reason: "x"},
}

// notJustified are checks that find nothing wrong: the test profiles' only
// symptom, the check that justifies their rollback, is false.
var notJustified = []check.Result{{Name: "oom_killed_new_revision", Version: "1", Value: check.False, Reason: "x"}, predicates[1]}

// notices counts the escalation notices queued for an incident.
func (e *env) notices(t *testing.T, incidentID string) int {
	t.Helper()
	return pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'incidentID' = $2`, store.EscalationArgs{}.Kind(), incidentID)
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

// collectedLater records item for the incident id, as evidence an
// investigator asked for after the checks ran.
func (e *env) collectedLater(t *testing.T, id string, item evidence.Item) {
	t.Helper()
	if err := e.store.InTx(t.Context(), func(tx pgx.Tx) error { return e.store.InsertEvidence(t.Context(), tx, id, "", nil, item) }); err != nil {
		t.Fatal(err)
	}
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

// A fix policy forbids closes the incident without acting when its checks
// find nothing wrong. While they find the failure, a person takes it over
// instead.
func TestGovern_Denied(t *testing.T) {
	for _, tt := range []struct {
		name    string
		checks  []check.Result
		flagged bool
		want    incident.State
	}{
		{"forbidden by a rule", notJustified, false, incident.ClosedNoAction},
		// The check is decided again over evidence the investigator asked for
		// after it first ran. The new version still runs out of memory.
		{"evidence holds instructions", append(slices.Clone(predicates), check.Result{Name: check.EvidenceHasInstructions, Version: "1", Value: check.False, Reason: "x"}), true, incident.AwaitingSuggestion},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			deny := decisions(t, "deny")
			id, p := e.diagnosed(t, "rollout-regression", 1, tt.checks)
			if tt.flagged {
				e.collectedLater(t, id, evidence.Item{
					ID: "0192f0c1-0000-7000-8000-00000000000b", Template: evidence.ContainerLogsTail, Data: []byte(`{}`), CollectedAt: now,
					Classification: evidence.ClassificationSensitive, Suspicious: []evidence.Suspicion{{Reason: evidence.ReasonFixOrder, At: "/logs"}},
				})
			}
			e.govern(t, p)

			if tt.flagged {
				results, err := e.store.Predicates(t.Context(), id)
				if err != nil || results[len(results)-1].Value != check.True {
					t.Errorf("predicates = %+v (%v), want %s true", results, err, check.EvidenceHasInstructions)
				}
			}
			if a := e.action(t, id); a.State != store.ActionDenied || a.ExpiresAt != nil {
				t.Errorf("action = %s, want denied", a.State)
			}
			if got := e.state(t, id); got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
			if referred := e.notices(t, id) == 1; referred != (tt.want == incident.AwaitingSuggestion) {
				t.Errorf("notice sent = %v, want one only when a person takes it over", referred)
			}
			if got := deny(); got != 1 {
				t.Errorf("counted %v denials, want 1", got)
			}
		})
	}
}

// A fix policy allows that Themis can't apply is only recommended, for the
// same reasons a person reads: Argo CD manages the workload, or the executor
// may not apply it. The incident waits for a person to make the change, and
// they are told. Only GitOps counts as a GitOps refusal.
func TestGovern_OnlyRecommends(t *testing.T) {
	for _, tt := range []struct {
		name     string
		gitOps   bool
		reach    *governance.Reach
		rule     string
		reason   string
		refusals float64
	}{
		{"GitOps owns the target", true, &governance.Reach{}, governance.GitOpsRule, "Argo CD manages this workload. Apply the change in Git.", 1},
		{"the executor is off", false, &governance.Reach{}, governance.ExecutorRule, "The executor is off. Apply the change yourself.", 0},
		{
			"the executor may not change the namespace", false, &governance.Reach{Executor: &config.Executor{Namespaces: []string{"payments"}}}, governance.ExecutorRule,
			"The executor may not change workloads in %s, as executor.namespaces doesn't list it. Apply the change yourself.", 0,
		},
		{
			"the executor may not apply the fix", false, &governance.Reach{Executor: &config.Executor{Namespaces: []string{"%s"}, Fixes: []string{"deployment.set-resources"}}}, governance.ExecutorRule,
			"The executor may not apply a rollback, as executor.fixes doesn't list deployment.rollback-to-revision. Apply the change yourself.", 0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.gitOps {
				argoManaged(t, e.target)
			}
			if x := tt.reach.Executor; x != nil {
				x.Namespaces = slices.Clone(x.Namespaces)
				for i := range x.Namespaces {
					x.Namespaces[i] = strings.ReplaceAll(x.Namespaces[i], "%s", e.target.Namespace)
				}
			}
			e.worker.Reach = tt.reach
			refusals := metrictest.Delta(t, "themis_gitops_refusals_total", nil)
			id, p := e.diagnosed(t, "rollout-regression", 1, predicates)
			e.govern(t, p)

			a := e.action(t, id)
			want := strings.ReplaceAll(tt.reason, "%s", e.target.Namespace)
			if a.State != store.ActionDenied || a.PolicyID != tt.rule || a.DecisionReason != want || a.ExpiresAt != nil {
				t.Errorf("action = %s by %q (%s), want denied by %q (%s)", a.State, a.PolicyID, a.DecisionReason, tt.rule, want)
			}
			if got := e.state(t, id); got != incident.Recommended {
				t.Errorf("state = %s, want recommended", got)
			}
			if got := e.notices(t, id); got != 1 {
				t.Errorf("%d notices, want one: a person makes the change", got)
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

// A suggestion Themis can't turn into a fix closes the incident without one
// when its checks find nothing wrong.
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
			id, p := e.diagnosed(t, tt.profile, tt.revision, notJustified)
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

// The investigator's fix refused while the checks still find the failure
// never closes the incident: a person takes it over, told why the fix was
// refused and what the checks support, and Themis waits for the fix they
// suggest. The refused suggestion never goes again.
func TestGovern_RefusedFixWhileItStillFails(t *testing.T) {
	e := newEnv(t)
	id, p := e.diagnosed(t, "rollout-regression", 9, predicates)
	e.govern(t, p)
	if got := e.state(t, id); got != incident.AwaitingSuggestion {
		t.Fatalf("state = %s, want awaiting_suggestion", got)
	}
	timeline, err := e.store.Transitions(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Revision 9 no longer exists, so Themis can't roll back to it. The checks support a rollback (the new version runs out of memory). Referred to a person."
	if got := timeline[len(timeline)-1].Reason; got != want {
		t.Errorf("timeline says %q, want %q", got, want)
	}
	if proposal, err := e.store.Proposal(t.Context(), e.store.DB(), p); err != nil || proposal.Status != store.ProposalRejected {
		t.Errorf("proposal = %s (%v), want rejected", proposal.Status, err)
	}
	if n := e.notices(t, id); n != 1 {
		t.Errorf("%d escalation notices, want 1", n)
	}
}

// A fix a person suggested that Themis refuses leaves the incident waiting
// for another, with the refusal on its timeline and the suggestion kept as
// refused, so it never goes again. So does the investigator's once a person
// denied a fix for the incident. A fix Themis only recommends, as GitOps owns
// the workload, waits for a person to make the change instead.
func TestGovern_RefusedWhileAPersonWorksOnIt(t *testing.T) {
	for _, tt := range []struct {
		name     string
		revision int64
		checks   []check.Result
		gitOps   bool
		by       string
		want     incident.State
		// refused says why the suggestion was refused before an action.
		refused string
	}{
		{name: "forbidden by a rule", revision: 1, checks: notJustified, by: "person", want: incident.AwaitingSuggestion},
		{name: "revision missing", revision: 9, checks: predicates, by: "person", want: incident.AwaitingSuggestion, refused: "Revision 9 no longer exists, so Themis can't roll back to it."},
		{name: "the investigator's, after a denial", revision: 1, checks: notJustified, by: "denied", want: incident.AwaitingSuggestion},
		{name: "GitOps owns the target", revision: 1, checks: predicates, gitOps: true, by: "person", want: incident.Recommended},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.gitOps {
				argoManaged(t, e.target)
			}
			id, p := e.diagnosed(t, "rollout-regression", tt.revision, tt.checks)
			if tt.by == "person" {
				e.suggestedBy(t, p, "person:alex")
			} else {
				e.deniedBefore(t, id)
			}
			e.govern(t, p)
			if got := e.state(t, id); got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
			proposal, err := e.store.Proposal(t.Context(), e.store.DB(), p)
			if err != nil {
				t.Fatal(err)
			}
			timeline, err := e.store.Transitions(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			last := timeline[len(timeline)-1]
			if tt.refused != "" && (proposal.Status != store.ProposalRejected || proposal.Message != tt.refused || last.Reason != tt.refused) {
				t.Errorf("proposal %s (%q), timeline says %q; want refused, saying %q", proposal.Status, proposal.Message, last.Reason, tt.refused)
			}
			if tt.refused == "" && tt.want == incident.AwaitingSuggestion && !strings.HasPrefix(last.Reason, "Refused.") {
				t.Errorf("timeline says %q, want the refusal", last.Reason)
			}
		})
	}
}

// deniedBefore records a person's denial of an earlier fix for the incident.
func (e *env) deniedBefore(t *testing.T, incidentID string) {
	t.Helper()
	ctx := t.Context()
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := e.store.InsertProposal(ctx, tx, store.Proposal{
			IncidentID: incidentID, ClientProposalID: "earlier", Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Target: e.target, Params: map[string]any{"revision": int64(2)}, Provenance: map[string]string{"source": "person:alex"}, Status: store.ProposalAccepted,
		})
		if err != nil {
			return err
		}
		a, err := e.store.InsertAction(ctx, tx, store.Action{
			IncidentID: incidentID, ProposalID: p.ID, Capability: "deployment.rollback-to-revision", CapabilityVersion: "1",
			Params: map[string]any{"revision": int64(2)}, State: store.ActionAwaitingApproval, Digest: "sha256:earlier",
		})
		if err != nil {
			return err
		}
		_, err = e.store.Deny(ctx, tx, a, "alice", "not this one", now)
		return err
	})
	if err != nil {
		t.Fatal(err)
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
	// The new version still runs out of memory: a person takes it over.
	if got := e.state(t, id); got != incident.AwaitingSuggestion {
		t.Errorf("state = %s, want awaiting_suggestion", got)
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
