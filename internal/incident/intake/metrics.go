package intake

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Intake metrics, shared by every source.
var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_intake_requests_total",
		Help: "Intake HTTP requests, by source and response status code.",
	}, []string{"source", "code"})

	alertsReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_intake_alerts_received_total",
		Help: "Alerts stored at intake, by source and whether they could be translated (accepted) or not (invalid).",
	}, []string{"source", "result"})

	truncatedAlerts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_intake_truncated_alerts_total",
		Help: "Alerts a source dropped before sending them to Themis, for example beyond Alertmanager's max_alerts.",
	}, []string{"source"})

	alertsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_intake_alerts_processed_total",
		Help: "Alerts processed, by outcome (created, joined, resolved, stale, rejected, retried) and, for rejections, reason.",
	}, []string{"outcome", "reason"})

	refused = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "themis_intake_refused_total",
		Help: "Intake requests refused before their alerts were read, by source and reason (no_admin_account).",
	}, []string{"source", "reason"})
)

// Why an intake request is refused before its alerts are read.
const (
	// RefusedNoAdminAccount: no admin account exists, so Themis takes no
	// alerts.
	RefusedNoAdminAccount = "no_admin_account"
)

// Outcomes of processing an alert.
const (
	outcomeCreated  = "created"
	outcomeJoined   = "joined"
	outcomeResolved = "resolved"
	outcomeRejected = "rejected"
	outcomeRetried  = "retried"
	outcomeStale    = "stale"
)

func init() {
	metrics.Registry.MustRegister(requests, alertsReceived, truncatedAlerts, alertsProcessed, refused)
}

// ObserveRefused counts an intake request refused for reason before its
// alerts were read.
func ObserveRefused(source, reason string) {
	refused.WithLabelValues(source, reason).Inc()
}

// ObserveRequest counts an intake HTTP request answered with code.
func ObserveRequest(source string, code int) {
	requests.WithLabelValues(source, strconv.Itoa(code)).Inc()
}

// ObserveTruncated counts n alerts a source dropped before sending.
func ObserveTruncated(source string, n uint64) {
	truncatedAlerts.WithLabelValues(source).Add(float64(n))
}
