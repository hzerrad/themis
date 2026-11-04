package notification_test

import (
	"net/http"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/notification"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

func TestMetrics_Notifications(t *testing.T) {
	for code, result := range map[int]string{http.StatusNoContent: "delivered", http.StatusBadGateway: "failed"} {
		t.Run(result, func(t *testing.T) {
			s, _ := pgtest.NewStore(t)
			srv, _ := receiver(t, code)
			counted := metrictest.Delta(t, "themis_notifications_total", map[string]string{"result": result})
			err := (&notification.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URL: srv.URL}}}).Notify(t.Context(), escalated(t, s))
			// A failing receiver is retried; River backs off between attempts.
			if (err == nil) != (result == "delivered") {
				t.Errorf("Notify = %v, want an error only when delivery failed", err)
			}
			if got := counted(); got != 1 {
				t.Errorf("counted %v, want 1", got)
			}
		})
	}
}
