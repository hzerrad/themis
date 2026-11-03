package grpcapi

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

var (
	proposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_proposals_total",
		Help: "Proposals stored, by result: accepted or rejected.",
	}, []string{"result"})

	externalProposalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_external_proposals_total",
		Help: "Proposals stored from outside Themis, by result: accepted or rejected.",
	}, []string{"result"})

	tokenReviewsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_token_reviews_total",
		Help: "Callers' tokens sent to the Kubernetes API server, by result: accepted, refused or failed.",
	}, []string{"result"})

	tokenReviewCacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "themis_token_review_cache_hits_total",
		Help: "Callers' tokens answered from a recent review instead of the API server.",
	})

	requestsLimitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_requests_limited_total",
		Help: "Requests from outside Themis refused for coming too often, by kind: address, reads or suggestions.",
	}, []string{"kind"})

	abstentionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_abstentions_total",
		Help: "Investigations that couldn't decide, by kind: blind when evidence Themis could have had was missing, ambiguous when it saw everything and found no clear answer.",
	}, []string{"kind"})

	safetyEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_safety_events_total",
		Help: "Proposals rejected for naming another target (wrong_target), a capability the profile does not allow (unknown_capability) or evidence that was never collected (unknown_evidence).",
	}, []string{"kind"})
)

// safetyKinds are the rejections that are safety events.
var safetyKinds = map[themisv1.RejectionReason]string{
	themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH:        "wrong_target",
	themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED: "unknown_capability",
	themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE:       "unknown_evidence",
}

// budgetReasons end an investigation for running out of its budget.
var budgetReasons = map[themisv1.InvestigationFailureReason]bool{
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:  true,
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED: true,
}

func init() {
	metrics.Registry.MustRegister(proposalsTotal, externalProposalsTotal, abstentionsTotal, safetyEventsTotal,
		tokenReviewsTotal, tokenReviewCacheHitsTotal, requestsLimitedTotal)
	for _, r := range []string{"accepted", "refused", "failed"} {
		tokenReviewsTotal.WithLabelValues(r)
	}
	for _, l := range []string{"address", "reads", "suggestions"} {
		requestsLimitedTotal.WithLabelValues(l)
	}
	externalProposalsTotal.WithLabelValues("accepted")
	externalProposalsTotal.WithLabelValues("rejected")
	abstentionsTotal.WithLabelValues("blind")
	abstentionsTotal.WithLabelValues("ambiguous")
	proposalsTotal.WithLabelValues("accepted")
	proposalsTotal.WithLabelValues("rejected")
	for _, k := range safetyKinds {
		safetyEventsTotal.WithLabelValues(k)
	}
}
