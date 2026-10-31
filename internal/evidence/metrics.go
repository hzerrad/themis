package evidence

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	collectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_evidence_collected_total",
		Help: "Evidence items collected, by template.",
	}, []string{"template"})

	truncatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_evidence_truncated_total",
		Help: "Evidence items cut to their template's bound, by template.",
	}, []string{"template"})

	redactionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_redactions_total",
		Help: "Secrets replaced in collected evidence, by template.",
	}, []string{"template"})
)

func init() {
	metrics.Registry.MustRegister(collectedTotal, truncatedTotal, redactionsTotal)
}

// initMetrics reports zero for name until it collects.
func initMetrics(name string) {
	collectedTotal.WithLabelValues(name)
	truncatedTotal.WithLabelValues(name)
	redactionsTotal.WithLabelValues(name)
}

func countItem(item Item) {
	collectedTotal.WithLabelValues(item.Template).Inc()
	if item.Truncated {
		truncatedTotal.WithLabelValues(item.Template).Inc()
	}
	redactionsTotal.WithLabelValues(item.Template).Add(float64(item.Redactions))
}
