package grpcapi

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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
)

func init() {
	metrics.Registry.MustRegister(tokenReviewsTotal, tokenReviewCacheHitsTotal)
	for _, r := range []string{"accepted", "refused", "failed"} {
		tokenReviewsTotal.WithLabelValues(r)
	}
}
