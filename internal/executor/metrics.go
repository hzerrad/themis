package executor

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/hzerrad/themis/internal/rollback"
	"github.com/hzerrad/themis/internal/telemetry"
)

// resultDryRunFailed is a Failed result refused by the API server's dry run.
const resultDryRunFailed = "dry_run_failed"

// capabilities the executor can run.
var capabilities = telemetry.Bounded{rollback.Name}

var actionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_actions_total",
	Help: "Actions executed, by capability and result: succeeded, stale, dry_run_failed, failed or ambiguous.",
}, []string{"capability", "result"})

func init() {
	metrics.Registry.MustRegister(actionsTotal)
	for _, c := range capabilities {
		for _, r := range []string{string(Succeeded), string(Stale), resultDryRunFailed, string(Failed), string(Ambiguous)} {
			actionsTotal.WithLabelValues(c, r)
		}
	}
}

func (r Result) label() string {
	if r.dryRunFailed {
		return resultDryRunFailed
	}
	return string(r.Status)
}
