package verify

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var verificationOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_verification_outcomes_total",
	Help: "Final outcomes of verifying an executed action: resolved, contained, unchanged, worsened or inconclusive, which includes a target that could not be observed.",
}, []string{"outcome"})

func init() {
	metrics.Registry.MustRegister(verificationOutcomes)
	for o := range outcomes {
		verificationOutcomes.WithLabelValues(string(o))
	}
}
