package governance_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/testutil/metrictest"
)

func decisions(t *testing.T, decision string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_policy_decisions_total", map[string]string{"decision": decision})
}

// A repeated govern job decides once.
func TestMetrics_PolicyDecisionCountedOnce(t *testing.T) {
	e := newEnv(t)
	approval := decisions(t, "require_approval")
	_, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	e.govern(t, p)
	if got := approval(); got != 1 {
		t.Errorf("counted %v require_approval decisions, want 1", got)
	}
}

func TestMetrics_GitOpsRefusal(t *testing.T) {
	e := newEnv(t)
	var d appsv1.Deployment
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: e.target.Namespace, Name: "checkout"}, &d); err != nil {
		t.Fatal(err)
	}
	d.Annotations["argocd.argoproj.io/tracking-id"] = "shop:apps/Deployment:shop/checkout"
	if err := k8s.Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
	deny := decisions(t, "deny")
	refusals := metrictest.Delta(t, "themis_gitops_refusals_total", nil)
	_, p := e.diagnosed(t, "rollout-regression", 1, predicates)
	e.govern(t, p)
	if got := deny(); got != 1 {
		t.Errorf("counted %v denials, want 1", got)
	}
	if got := refusals(); got != 1 {
		t.Errorf("counted %v GitOps refusals, want 1", got)
	}
}

// A denial of a target no GitOps controller manages is not a GitOps refusal.
func TestMetrics_OtherDenialIsNotAGitOpsRefusal(t *testing.T) {
	e := newEnv(t)
	refusals := metrictest.Delta(t, "themis_gitops_refusals_total", nil)
	_, p := e.diagnosed(t, "rollout-regression", 1, unknownPredicates)
	e.govern(t, p)
	if got := refusals(); got != 0 {
		t.Errorf("counted %v GitOps refusals, want 0", got)
	}
}
