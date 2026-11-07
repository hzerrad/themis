package governance_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/store"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// renewedAt is when a person asks again: after the expiry sweep.
var renewedAt = now.Add(config.DefaultApprovalTimeout + 2*time.Hour)

// expired creates an incident whose fix's approval expired with no
// decision, so it waits for a fix, and has the worker's clock read
// renewedAt from then on.
func (e *env) expired(t *testing.T) store.Action {
	t.Helper()
	a := e.awaiting(t)
	if err := e.expireWorker().Expire(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.worker.Now = func() time.Time { return renewedAt }
	return a
}

// counts are what an incident keeps that a renewal adds to: changes of
// state, actions, evidence, and investigations, the only way the model is
// asked.
func (e *env) counts(t *testing.T, id string) []int {
	t.Helper()
	return []int{
		pgtest.Count(t, e.pool, `SELECT count(*) FROM transitions WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM actions WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM evidence_items WHERE incident_id = $1`, id),
		pgtest.Count(t, e.pool, `SELECT count(*) FROM investigations WHERE incident_id = $1`, id),
	}
}

// Asking again for an approval that expired, with the workload as the fix
// was planned against and the checks still justifying it on evidence read
// now, puts the same change to a person again, with a new window, as a new
// action: the expired one stays expired. No investigation starts, so the
// model isn't asked. The checks keep their new answers, and what was read
// joins the evidence. The new action can be approved.
func TestRenew_AsksAgain(t *testing.T) {
	e := newEnv(t)
	a := e.expired(t)
	renewed, approval := approvals(t, "renewed"), decisions(t, "require_approval")
	before := e.counts(t, a.IncidentID)

	got, err := e.worker.Renew(t.Context(), a.ID, "alice")
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if got.ID == a.ID || got.State != store.ActionAwaitingApproval || got.ExpiresAt == nil || !got.ExpiresAt.Equal(renewedAt.Add(config.DefaultApprovalTimeout)) {
		t.Errorf("renewed = %s %s, expiring %v; want a new action awaiting approval until %v", got.ID, got.State, got.ExpiresAt, renewedAt.Add(config.DefaultApprovalTimeout))
	}
	if got.Digest != a.Digest || got.ProposalID != a.ProposalID {
		t.Errorf("renewed binds %s for %s, want the same change, %s for %s", got.Digest, got.ProposalID, a.Digest, a.ProposalID)
	}
	if old, err := e.store.Action(t.Context(), e.store.DB(), a.ID); err != nil || old.State != store.ActionExpired {
		t.Errorf("the expired action = %s (%v), want still expired", old.State, err)
	}
	if got := e.state(t, a.IncidentID); got != incident.AwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got)
	}
	timeline, err := e.store.Transitions(t.Context(), a.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	const says = "Approval asked again by alice. The checks still support the rollback."
	for _, tr := range timeline[before[0]:] {
		if tr.Reason != says || tr.Actor != "alice" || tr.To == incident.Investigating {
			t.Errorf("then %s → %s: %q by %s, want %q by alice, with no investigation", tr.From, tr.To, tr.Reason, tr.Actor, says)
		}
	}
	after := e.counts(t, a.IncidentID)
	if after[1] != before[1]+1 || after[2] <= before[2] || after[3] != before[3] {
		t.Errorf("actions, evidence and investigations went from %v to %v; want one more action, more evidence, and no investigation", before[1:], after[1:])
	}
	results, err := e.store.Predicates(t.Context(), a.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if r := results[0]; r.Name != "oom_killed_new_revision" || r.Value != check.True || r.Reason == predicates[0].Reason {
		t.Errorf("%s = %s (%q), want decided again from the pods as they are", r.Name, r.Value, r.Reason)
	}
	if n, d := renewed(), approval(); n != 1 || d != 1 {
		t.Errorf("counted %v renewals and %v policy decisions, want 1 each", n, d)
	}
	if _, err := governance.Approve(t.Context(), e.store, got.ID, got.Digest, "bob", renewedAt, false); err != nil {
		t.Errorf("approving the fix asked again: %v", err)
	}
}

// Asking again is refused, saying why, and keeps nothing, when the fix may
// no longer be what a person should approve: the workload changed since it
// was planned, the checks decided again no longer justify it, policy
// forbids it now, its approval never expired, the incident moved on, or
// another fix is under way on the workload.
func TestRenew_Refused(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setUp makes the fix's approval expire, or not, and changes what
		// the test is about.
		setUp func(t *testing.T, e *env) store.Action
		says  string
		state incident.State
	}{
		{
			name: "the workload changed",
			setUp: func(t *testing.T, e *env) store.Action {
				a := e.expired(t)
				edit(t, e.target, func(d *appsv1.Deployment) { d.Spec.Template.Annotations = map[string]string{"restartedAt": "now"} })
				return a
			},
			says:  "Refused. checkout changed since the fix was planned. Investigate again.",
			state: incident.AwaitingSuggestion,
		},
		{
			name: "the checks no longer justify it",
			setUp: func(t *testing.T, e *env) store.Action {
				a := e.expired(t)
				// The new version's only pod, the one OOMKilled, is gone.
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: e.target.Namespace, Name: "checkout-7d9f8b6c4-x2kqp"}}
				if err := k8s.Delete(t.Context(), pod, client.GracePeriodSeconds(0)); err != nil {
					t.Fatal(err)
				}
				return a
			},
			says:  "Refused. The checks no longer support the rollback. The rollout-regression profile allows a rollback only when the new version runs out of memory, and the checks couldn't tell (no pods of the current revision).",
			state: incident.AwaitingSuggestion,
		},
		{
			name: "policy forbids it now",
			setUp: func(t *testing.T, e *env) store.Action {
				a := e.expired(t)
				p := profile("rollout-regression")
				p.Policy.Rules = append([]config.PolicyRule{{Name: "frozen", When: "true", Decision: config.DecisionDeny}}, p.Policy.Rules...)
				e.worker.Config = config.Config{EnrolledNamespaces: []string{e.target.Namespace}, Profiles: []config.Profile{p}}
				return a
			},
			says:  "Refused. Policy rule frozen of the rollout-regression profile forbids this fix.",
			state: incident.AwaitingSuggestion,
		},
		{
			name:  "its approval didn't expire",
			setUp: func(t *testing.T, e *env) store.Action { return e.awaiting(t) },
			says:  "Refused. Only a fix whose approval expired with no decision can be asked for again.",
			state: incident.AwaitingApproval,
		},
		{
			name: "the incident moved on",
			setUp: func(t *testing.T, e *env) store.Action {
				a := e.expired(t)
				if _, err := e.pool.Exec(t.Context(), `UPDATE incidents SET state = 'investigating' WHERE id = $1`, a.IncidentID); err != nil {
					t.Fatal(err)
				}
				return a
			},
			says:  "Refused. The incident isn't waiting for a fix.",
			state: incident.Investigating,
		},
		{
			name: "another fix is under way on the workload",
			setUp: func(t *testing.T, e *env) store.Action {
				a := e.expired(t)
				_, p := e.diagnosed(t, "other", 1, predicates)
				e.govern(t, p)
				return a
			},
			says:  "Refused. Another fix is already in progress on this workload.",
			state: incident.AwaitingSuggestion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			a := tc.setUp(t, e)
			refused := approvals(t, "renewal_refused")
			before := e.counts(t, a.IncidentID)

			_, err := e.worker.Renew(t.Context(), a.ID, "alice")
			var r *governance.Refusal
			if !errors.As(err, &r) || r.Reason != tc.says {
				t.Fatalf("Renew = %v, want refused: %q", err, tc.says)
			}
			if got := e.state(t, a.IncidentID); got != tc.state {
				t.Errorf("state = %s, want %s", got, tc.state)
			}
			if after := e.counts(t, a.IncidentID); !slices.Equal(after, before) {
				t.Errorf("transitions, actions, evidence and investigations went from %v to %v, want unchanged", before, after)
			}
			if got := refused(); got != 1 {
				t.Errorf("counted %v refusals, want 1", got)
			}
		})
	}
}

// Whether the page offers to ask again is decided from what Themis keeps,
// by the same rule asking again is: the incident's latest fix expired with
// no decision, it waits for a fix since, and its profiles still offer the
// fix, which Themis can carry out.
func TestRenewable(t *testing.T) {
	e := newEnv(t)
	waiting := store.Incident{State: incident.AwaitingSuggestion, Profile: "rollout-regression"}
	fix := func(id string, state store.ActionState) store.Action {
		return store.Action{ID: id, Capability: "deployment.rollback-to-revision", State: state}
	}
	for _, tc := range []struct {
		name    string
		in      store.Incident
		actions []store.Action
		want    bool
	}{
		{"its latest fix expired", waiting, []store.Action{fix("a", store.ActionDenied), fix("b", store.ActionExpired)}, true},
		{"no fix", waiting, nil, false},
		{"its latest fix was denied", waiting, []store.Action{fix("a", store.ActionExpired), fix("b", store.ActionDenied)}, false},
		{"its fix awaits approval", store.Incident{State: incident.AwaitingApproval, Profile: "rollout-regression"}, []store.Action{fix("a", store.ActionAwaitingApproval)}, false},
		{"it is investigated again", store.Incident{State: incident.Investigating, Profile: "rollout-regression"}, []store.Action{fix("a", store.ActionExpired)}, false},
		{"its profile is gone", store.Incident{State: incident.AwaitingSuggestion, Profile: "removed"}, []store.Action{fix("a", store.ActionExpired)}, false},
		{"Themis can't carry it out", waiting, []store.Action{{ID: "a", Capability: "deployment.teleport", State: store.ActionExpired}}, false},
	} {
		if got := e.worker.Renewable(tc.in, tc.actions); got != tc.want {
			t.Errorf("%s: Renewable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// edit changes the target's Deployment.
func edit(t *testing.T, target incident.TargetRef, change func(*appsv1.Deployment)) {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	change(&d)
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
}
