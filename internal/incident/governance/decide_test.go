package governance_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/digest"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/incident/governance/policy"
)

// cluster loads scenario 1 into a namespace of its own and returns its
// target: checkout runs revision 2, and keeps revision 1.
func cluster(t *testing.T) incident.TargetRef {
	t.Helper()
	ns := k8s.NewNamespace(t)
	k8s.Load(t, ns, scenario1)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "checkout", UID: string(d.UID)}
}

// shippedProfile is the rollout-regression scenario pack's profile, so the
// decisions below are the rules Themis ships, not ones written for a test.
func shippedProfile(t *testing.T) config.Profile {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Profile("rollout-regression")
	if !ok {
		t.Fatal("no rollout-regression pack")
	}
	return p
}

func facts(onset check.Truth) []check.Result {
	return []check.Result{
		{Name: "oom_killed_new_revision", Version: "1", Value: check.True, Reason: "x"},
		{Name: governance.ManagedCheck, Version: "1", Value: check.False, Reason: "x"},
		{Name: "failure_onset_after_rollout", Version: "1", Value: onset, Reason: "x"},
		{Name: "config_changed_before_failure", Version: "1", Value: check.False, Reason: "x"},
		{Name: "rollout_recent", Version: "1", Value: check.True, Reason: "x"},
		{Name: "prior_revision_was_healthy", Version: "1", Value: check.True, Reason: "x"},
	}
}

// set returns facts with the result at i changed to v.
func set(results []check.Result, i int, v check.Truth) []check.Result {
	results[i].Value = v
	return results
}

// argoManaged marks target as Argo CD's.
func argoManaged(t *testing.T, target incident.TargetRef) {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: target.Namespace, Name: target.Name}, &d); err != nil {
		t.Fatal(err)
	}
	d.Annotations["argocd.argoproj.io/tracking-id"] = "shop:apps/Deployment:shop/checkout"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
}

var investigator = governance.Suggester{Source: "themis-intelligence", Authenticated: true}

// A fix is planned against the live cluster and decided by its profile's
// policy, the same for a suggestion, a restore and the evaluation. The
// shipped policy rolls back only a rollout that is to blame, and even then
// only with a person's approval.
func TestDecide(t *testing.T) {
	shipped := shippedProfile(t)
	outsiders := profile("outsiders")
	outsiders.Policy.Rules = []config.PolicyRule{{Name: "no-unproven-outsiders", When: `proposal.external == "true" && proposal.authenticated != "true"`, Decision: config.DecisionDeny}}
	for _, tt := range []struct {
		name     string
		profile  config.Profile
		checks   []check.Result
		revision int64
		by       governance.Suggester
		restores string
		gitOps   bool

		want    policy.Outcome
		rule    string
		refusal string
	}{
		// The control: a bad rollout, with every fact known, waits for approval.
		{name: "bad rollout", checks: facts(check.True), want: policy.RequireApproval, rule: "rollback-needs-approval"},
		// A new version that crashes, or never starts, while the one before it
		// was healthy is a bad rollout too, though nothing ran out of memory.
		{name: "bad rollout without OOM", checks: set(facts(check.True), 0, check.False), want: policy.RequireApproval, rule: "rollback-needs-approval"},
		// The crash loop began before the rollout: rolling back would not help.
		{name: "crash loop older than the rollout", checks: facts(check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// Without the evidence that says when the failures started, no rollback.
		{name: "missing evidence", checks: facts(check.Unknown), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// A referenced ConfigMap changed between the rollout and the failures:
		// rolling back wouldn't undo it.
		{name: "config changed before the failure", checks: set(facts(check.True), 3, check.True), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		{name: "config change unknown", checks: set(facts(check.True), 3, check.Unknown), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// A failure long after the rollout isn't blamed on it.
		{name: "old rollout", checks: set(facts(check.True), 4, check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// No memory kill and no healthy version to go back to.
		{name: "no sign of a bad revision", checks: set(set(facts(check.True), 0, check.False), 5, check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		{name: "prior revision's health unknown", checks: set(set(facts(check.True), 0, check.False), 5, check.Unknown), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// GitOps ownership is read live: a stale check does not hide it.
		{name: "GitOps owns the target", checks: facts(check.True), gitOps: true, want: policy.Deny, rule: "gitops-owns-the-target"},
		// Putting a fix back needs no approval, whatever the checks say of a fix.
		{name: "putting a fix back", checks: facts(check.False), restores: "the-fix", want: policy.Allow},
		// Rules see who suggested the fix.
		{name: "unproven outsider", profile: outsiders, checks: facts(check.True), by: governance.Suggester{Source: "serviceaccount:agents/triage", External: true}, want: policy.Deny, rule: "no-unproven-outsiders"},
		// A fix that can't go ahead is refused before policy sees it.
		{name: "revision gone", checks: facts(check.True), revision: 9, refusal: "Revision 9 no longer exists, so Themis can't roll back to it."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := cluster(t)
			if tt.gitOps {
				argoManaged(t, target)
			}
			p := shipped
			if tt.profile.Name != "" {
				p = tt.profile
			}
			revision, by := tt.revision, tt.by
			if revision == 0 {
				revision = 1
			}
			if by == (governance.Suggester{}) && tt.restores == "" {
				by = investigator
			}
			var decider governance.Decider
			d, err := decider.Decide(t.Context(), k8s, governance.Fix{
				Incident: "incident-1", Target: target, Profile: p, Checks: tt.checks,
				Capability: rollback.Capability{}, Params: map[string]any{"revision": revision},
				By: by, Restores: tt.restores,
			})
			if tt.refusal != "" {
				if refusal, ok := errors.AsType[*capability.Refusal](err); !ok || refusal.Reason != tt.refusal {
					t.Fatalf("err = %v, want the refusal %q", err, tt.refusal)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Decision.Outcome != tt.want || d.Decision.PolicyID != tt.rule {
				t.Errorf("decision = %s by %q (%s), want %s by %q", d.Decision.Outcome, d.Decision.PolicyID, d.Decision.Reason, tt.want, tt.rule)
			}
			a := d.Action
			if a.Decision != string(d.Decision.Outcome) || a.DecisionReason != d.Decision.Reason || a.PolicyDigest != d.Decision.PolicyDigest {
				t.Errorf("action records %s: %q, not the decision", a.Decision, a.DecisionReason)
			}
			if dg, err := digest.Compute(a.Fields()); err != nil || a.Digest != dg {
				t.Errorf("digest %s, want %s of the action's own fields (%v)", a.Digest, dg, err)
			}
			if (d.ManagedBy != "") != tt.gitOps {
				t.Errorf("managed by %q", d.ManagedBy)
			}
			if restores, _ := a.Restores(); restores != tt.restores {
				t.Errorf("restores %q, want %q", restores, tt.restores)
			}
			// The action comes from the cluster, not from what was suggested.
			if a.IncidentID != "incident-1" || a.Target.UID != target.UID || a.Target.SpecHash == "" || a.Params["templateHash"] == "" || a.Params["revision"] != int64(1) {
				t.Errorf("action identity = %+v, %v", a.Target, a.Params)
			}
			if a.BlastRadius["replicas"] != int64(3) || a.Risk != "medium" || a.Mark != a.Params["templateHash"] {
				t.Errorf("blast radius %v, risk %q, mark %q", a.BlastRadius, a.Risk, a.Mark)
			}
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
		})
	}
}

// Policy sees what a fix says of its change, and a change the cluster can't
// run is refused before policy decides.
func TestDecide_WhatTheChangeIs(t *testing.T) {
	p := profile("resources")
	p.Capabilities = []string{resources.Name}
	p.Policy.Rules = []config.PolicyRule{{Name: "no-doubling", When: `change["app.memory.limit.ratio"] == "2"`, Decision: config.DecisionDeny}}
	double := map[string]any{"containers": map[string]any{"app": map[string]any{"limits": map[string]any{"memory": "128Mi"}}}}
	for name, tc := range map[string]struct {
		maximum string
		rule    string
		refusal string
	}{
		"policy sees the ratio": {rule: "no-doubling"},
		"a LimitRange forbids it": {
			maximum: "100Mi",
			refusal: "The LimitRange limits in %s allows at most 100Mi of memory per container, and app would have a memory limit of 128Mi.",
		},
	} {
		target := cluster(t)
		if tc.maximum != "" {
			k8s.Create(t, &corev1.LimitRange{
				ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: target.Namespace},
				Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
					Type: corev1.LimitTypeContainer, Max: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(tc.maximum)},
				}}},
			})
		}
		var decider governance.Decider
		d, err := decider.Decide(t.Context(), k8s, governance.Fix{
			Incident: "incident-1", Target: target, Profile: p,
			Capability: resources.Capability{}, Params: double, By: investigator,
		})
		if tc.refusal != "" {
			want := fmt.Sprintf(tc.refusal, target.Namespace)
			if refusal, ok := errors.AsType[*capability.Refusal](err); !ok || refusal.Reason != want {
				t.Errorf("%s: err = %v, want the refusal %q", name, err, want)
			}
			continue
		}
		if err != nil || d.Decision.Outcome != policy.Deny || d.Decision.PolicyID != tc.rule {
			t.Errorf("%s: decision %+v, %v; want a deny by %s", name, d.Decision, err, tc.rule)
		}
	}
}
