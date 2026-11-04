package executor

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/capability"
	"github.com/hzerrad/themis/internal/telemetry"
)

// resultDryRunFailed is a Failed result refused by the API server's dry run.
const resultDryRunFailed = "dry_run_failed"

var actionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_actions_total",
	Help: "Actions executed, by capability and result: succeeded, stale, dry_run_failed, failed or ambiguous.",
}, []string{"capability", "result"})

func init() {
	metrics.Registry.MustRegister(actionsTotal)
}

// initMetrics starts the series of every fix at zero.
func initMetrics(fixes capability.Registry) {
	for _, c := range fixes.Names() {
		for _, r := range []string{string(Succeeded), string(Stale), resultDryRunFailed, string(Failed), string(Ambiguous)} {
			actionsTotal.WithLabelValues(c, r)
		}
	}
}

// capabilityLabel is a fix's name as a label: one this executor doesn't know
// is Other, so a stored name can't add series.
func (e *Executor) capabilityLabel(name string) string {
	return telemetry.Bounded(e.Fixes.Names()).Value(name)
}

func (r Result) label() string {
	if r.dryRunFailed {
		return resultDryRunFailed
	}
	return string(r.Status)
}
