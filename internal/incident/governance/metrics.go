package governance

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/incident/governance/policy"
)

var (
	policyDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_policy_decisions_total",
		Help: "Policy decisions on proposed actions: allow, require_approval, deny, or error when a rule failed to evaluate (which denies).",
	}, []string{"decision"})

	gitOpsRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "themis_gitops_refusals_total",
		Help: "Actions denied on a target that a GitOps controller manages.",
	})
)

func init() {
	metrics.Registry.MustRegister(policyDecisions, gitOpsRefusals)
	for _, d := range []string{string(policy.Allow), string(policy.RequireApproval), string(policy.Deny), decisionError} {
		policyDecisions.WithLabelValues(d)
	}
}

// decisionError is the label of a decision whose rule failed to evaluate.
const decisionError = "error"

func decisionLabel(d policy.Decision) string {
	if d.Failed {
		return decisionError
	}
	return string(d.Outcome)
}
