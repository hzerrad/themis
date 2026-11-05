package investigation

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var abstentionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_abstentions_total",
	Help: "Investigations that couldn't decide, by kind: blind when evidence Themis could have had was missing, ambiguous when it saw everything and found no clear answer.",
}, []string{"kind"})

func init() {
	metrics.Registry.MustRegister(abstentionsTotal)
	abstentionsTotal.WithLabelValues("blind")
	abstentionsTotal.WithLabelValues("ambiguous")
}
