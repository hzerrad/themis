package notify

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var notificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "themis_notifications_total",
	Help: "Attempts to post an escalation to the webhook, by result: delivered, or failed and retried.",
}, []string{"result"})

func init() {
	metrics.Registry.MustRegister(notificationsTotal)
	notificationsTotal.WithLabelValues("delivered")
	notificationsTotal.WithLabelValues("failed")
}

func countNotification(err error) {
	if err != nil {
		notificationsTotal.WithLabelValues("failed").Inc()
		return
	}
	notificationsTotal.WithLabelValues("delivered").Inc()
}
