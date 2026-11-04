package grpcapi

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

var (
	tokenReviewsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_token_reviews_total",
		Help: "Callers' tokens sent to the Kubernetes API server, by result: accepted, refused or failed.",
	}, []string{"result"})

	tokenReviewCacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "themis_token_review_cache_hits_total",
		Help: "Callers' tokens answered from a recent review instead of the API server.",
	})

	abstentionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_abstentions_total",
		Help: "Investigations that couldn't decide, by kind: blind when evidence Themis could have had was missing, ambiguous when it saw everything and found no clear answer.",
	}, []string{"kind"})
)

// budgetReasons end an investigation for running out of its budget.
var budgetReasons = map[themisv1.InvestigationFailureReason]bool{
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:  true,
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED: true,
}

func init() {
	metrics.Registry.MustRegister(abstentionsTotal, tokenReviewsTotal, tokenReviewCacheHitsTotal)
	for _, r := range []string{"accepted", "refused", "failed"} {
		tokenReviewsTotal.WithLabelValues(r)
	}
	abstentionsTotal.WithLabelValues("blind")
	abstentionsTotal.WithLabelValues("ambiguous")
}
