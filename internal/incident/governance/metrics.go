package governance

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/incident/governance/policy"
	"github.com/hzerrad/themis/internal/store"
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

	approvalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_approvals_total",
		Help: "Approval decisions: approved, denied, expired, and renewed for an approval asked again after one expired; and refused attempts: digest_mismatch for a digest other than the one shown, replayed for an action already decided, renewal_refused for an approval that couldn't be asked again.",
	}, []string{"result"})
)

// Results of an approval, or of an attempt the store refused.
const (
	resultApproved       = "approved"
	resultDenied         = "denied"
	resultExpired        = "expired"
	resultRenewed        = "renewed"
	resultDigestMismatch = "digest_mismatch"
	resultReplayed       = "replayed"
	resultRenewalRefused = "renewal_refused"
)

func init() {
	metrics.Registry.MustRegister(policyDecisions, gitOpsRefusals, approvalsTotal)
	for _, d := range []string{string(policy.Allow), string(policy.RequireApproval), string(policy.Deny), decisionError} {
		policyDecisions.WithLabelValues(d)
	}
	for _, r := range []string{resultApproved, resultDenied, resultExpired, resultRenewed, resultDigestMismatch, resultReplayed, resultRenewalRefused} {
		approvalsTotal.WithLabelValues(r)
	}
}

// countRefusal counts an attempt the store refused, which rolled back.
func countRefusal(err error) {
	switch {
	case errors.Is(err, store.ErrDigestMismatch):
		approvalsTotal.WithLabelValues(resultDigestMismatch).Inc()
	case errors.Is(err, store.ErrAlreadyDecided):
		approvalsTotal.WithLabelValues(resultReplayed).Inc()
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
