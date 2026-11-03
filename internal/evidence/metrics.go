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

	missingTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_missing_evidence_total",
		Help: "Evidence an investigation needed and couldn't have, by template and why: forbidden, not_in_profile, expired, not_found, budget_spent, data_ceiling, timeout, unavailable, or absent when there was nothing.",
	}, []string{"template", "cause"})
)

func init() {
	metrics.Registry.MustRegister(collectedTotal, truncatedTotal, redactionsTotal, missingTotal)
}

// initMetrics reports zero for name until it collects or goes missing, so
// every registered template has its series from the start.
func initMetrics(name string) {
	collectedTotal.WithLabelValues(name)
	truncatedTotal.WithLabelValues(name)
	redactionsTotal.WithLabelValues(name)
	for _, c := range GapCauses() {
		missingTotal.WithLabelValues(name, string(c))
	}
}

// CountGap counts evidence an investigation needed and couldn't have.
func CountGap(template string, cause GapCause) {
	missingTotal.WithLabelValues(template, string(cause)).Inc()
}

func countItem(item Item) {
	collectedTotal.WithLabelValues(item.Template).Inc()
	if item.Truncated {
		truncatedTotal.WithLabelValues(item.Template).Inc()
	}
	redactionsTotal.WithLabelValues(item.Template).Add(float64(item.Redactions))
}
