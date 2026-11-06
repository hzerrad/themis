package governance_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// shippedChecks are the checks Themis ships, which say what each finds.
func shippedChecks(t *testing.T) check.List {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	return cat.Checks
}

func facts(onset check.Truth) []check.Result {
	return []check.Result{
		{Name: "oom_killed_new_revision", Version: "1", Value: check.True, Reason: "x"},
		{Name: check.GitOpsManaged, Version: "1", Value: check.False, Reason: "x"},
		{Name: "failure_onset_after_rollout", Version: "1", Value: onset, Reason: "x"},
		{Name: "config_changed_before_failure", Version: "1", Value: check.False, Reason: "x"},
		{Name: "rollout_recent", Version: "1", Value: check.True, Reason: "x"},
		{Name: "earlier_revision_healthy", Version: "1", Value: check.True, Reason: "x"},
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

// A fix is planned against the live cluster and decided by its incident's
// profiles' policies, the same for a suggestion, a restore and the
// evaluation. The shipped policy rolls back only a rollout that is to blame,
// and even then only with a person's approval. With several profiles, every
// profile offering the fix decides it and the strictest decision wins; a
// profile's rules govern only its own fixes.
func TestDecide(t *testing.T) {
	shipped, words := shippedProfile(t), shippedChecks(t)
	outsiders := profile("outsiders")
	outsiders.Policy.Rules = []config.PolicyRule{{Name: "no-unproven-outsiders", When: `proposal.external == "true" && proposal.authenticated != "true"`, Decision: config.DecisionDeny}}
	// A profile offering another fix, whose rules forbid anything else, and
	// one offering none, whose rules allow anything.
	resourcesOnly := config.Profile{Name: "resources-only", Priority: 1, Capabilities: []string{resources.Name}, Policy: config.Policy{Rules: []config.PolicyRule{{Name: "only-set-resources", When: `action.capability != "` + resources.Name + `"`, Decision: config.DecisionDeny}}}}
	lenient := config.Profile{Name: "lenient", Priority: 1, Policy: config.Policy{Rules: []config.PolicyRule{{Name: "anything-goes", When: "true", Decision: config.DecisionAllow}}}}
	// Another profile offering the rollback, which never makes changes.
	watching := profile("watching")
	watching.Priority, watching.Policy.Mode = 1, config.ModeObserve
	for _, tt := range []struct {
		name     string
		profile  config.Profile
		also     []config.Profile
		checks   []check.Result
		revision int64
		by       governance.Suggester
		restores string
		gitOps   bool

		want    policy.Outcome
		rule    string
		reason  string
		refusal string
	}{
		// The control: a bad rollout, with every fact known, waits for approval.
		{name: "bad rollout", checks: facts(check.True), want: policy.RequireApproval, rule: "rollback-needs-approval"},
		// A new version that crashes, or never starts, while the one before it
		// was healthy is a bad rollout too, though nothing ran out of memory.
		{name: "bad rollout without OOM", checks: set(facts(check.True), 0, check.False), want: policy.RequireApproval, rule: "rollback-needs-approval"},
		// The crash loop began before the rollout: the check that justifies a
		// rollback is false, so Themis refuses it before any rule.
		// People read the reason: it says what the check finds, never its name.
		{name: "crash loop older than the rollout", checks: facts(check.False), want: policy.Deny, rule: governance.UnjustifiedRule, reason: "Refused. The rollout-regression profile allows a rollback only when the failures started after the rollout, and the checks found otherwise (x)."},
		// Without the evidence that says when the failures started, no rollback.
		{name: "missing evidence", checks: facts(check.Unknown), want: policy.Deny, rule: governance.UnjustifiedRule, reason: "Refused. The rollout-regression profile allows a rollback only when the failures started after the rollout, and the checks couldn't tell (x)."},
		// Every profile offering the fix must find it justified.
		{name: "another profile offering it doesn't find it justified", also: []config.Profile{watching}, checks: set(facts(check.True), 0, check.False), want: policy.Deny, rule: governance.UnjustifiedRule, reason: "Refused. The watching profile allows a rollback only when the new version runs out of memory, and the checks found otherwise (x)."},
		// A referenced ConfigMap changed between the rollout and the failures:
		// rolling back wouldn't undo it.
		{name: "config changed before the failure", checks: set(facts(check.True), 3, check.True), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		{name: "config change unknown", checks: set(facts(check.True), 3, check.Unknown), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// A failure long after the rollout isn't blamed on it.
		{name: "old rollout", checks: set(facts(check.True), 4, check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// No memory kill and no healthy version to go back to.
		{name: "no sign of a bad revision", checks: set(set(facts(check.True), 0, check.False), 5, check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		{name: "earlier revision's health unknown", checks: set(set(facts(check.True), 0, check.False), 5, check.Unknown), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// No earlier revision, or one that failed too: nothing healthy to go
		// back to, though the new version runs out of memory.
		{name: "no healthy earlier revision", checks: set(facts(check.True), 5, check.False), want: policy.Deny, rule: "rollback-needs-a-bad-rollout"},
		// Themis never changes a workload GitOps manages, or may manage, before
		// any profile's policy, which here would ask for approval: it only
		// recommends the fix. Ownership is read live too, so a stale check
		// doesn't hide it.
		{name: "GitOps owns the target", checks: facts(check.True), gitOps: true, want: policy.Deny, rule: governance.GitOpsRule, reason: "Argo CD manages this workload. Apply the change in Git."},
		{name: "the check says GitOps owns it", checks: set(facts(check.True), 1, check.True), want: policy.Deny, rule: governance.GitOpsRule, reason: "Argo CD or Flux manages this workload. Apply the change in Git."},
		{name: "GitOps may own it", checks: set(facts(check.True), 1, check.Unknown), want: policy.Deny, rule: governance.GitOpsRule, reason: "Themis couldn't tell whether Argo CD or Flux manages this workload. Apply the change in Git."},
		// Putting a fix back needs no approval, whatever the checks say of a fix,
		// unless GitOps owns the workload: then Themis changes nothing.
		{name: "putting a fix back", checks: facts(check.False), restores: "the-fix", want: policy.Allow},
		{name: "putting a fix back GitOps owns", checks: facts(check.False), restores: "the-fix", gitOps: true, want: policy.Deny, rule: governance.GitOpsRule},
		// Rules see who suggested the fix.
		{name: "unproven outsider", profile: outsiders, checks: facts(check.True), by: governance.Suggester{Source: "serviceaccount:agents/triage", External: true}, want: policy.Deny, rule: "no-unproven-outsiders"},
		// Another profile's rules govern only its own fixes.
		{name: "another profile's rules", also: []config.Profile{resourcesOnly}, checks: facts(check.True), want: policy.RequireApproval, rule: "rollback-needs-approval"},
		// Every profile offering the fix decides it, mode and all.
		{name: "another profile offering it never changes anything", also: []config.Profile{watching}, checks: facts(check.True), want: policy.Deny, rule: "approve"},
		// A fix none of the incident's profiles offers is forbidden.
		{name: "no profile offers it", profile: lenient, checks: facts(check.True), want: policy.Deny},
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
				Incident: "incident-1", Target: target, Profiles: append(config.Matched{p}, tt.also...), Checks: tt.checks, Words: words,
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
			if tt.reason != "" && d.Decision.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", d.Decision.Reason, tt.reason)
			}
			// However many profiles matched, a profile's decision says which
			// profile it was: only GitOps, and a fix none offers, are no
			// profile's.
			byProfile := d.Decision.PolicyID != governance.GitOpsRule && !strings.HasPrefix(d.Decision.Reason, "Refused. None of the incident's profiles")
			if byProfile && !regexp.MustCompile(` of the [a-z-]+ profile |[Tt]he [a-z-]+ profile('s| allows) `).MatchString(d.Decision.Reason) {
				t.Errorf("reason %q doesn't say which profile's policy decided", d.Decision.Reason)
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
	p.JustifiedBy = map[string][]string{resources.Name: {"oom_killed_new_revision"}}
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
			Incident: "incident-1", Target: target, Profiles: config.Matched{p}, Checks: facts(check.True),
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
