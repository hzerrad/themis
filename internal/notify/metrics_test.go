package notify_test

import (
	"net/http"
	"testing"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/notify"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

func TestMetrics_Notifications(t *testing.T) {
	for code, result := range map[int]string{http.StatusNoContent: "delivered", http.StatusBadGateway: "failed"} {
		t.Run(result, func(t *testing.T) {
			s, _ := pgtest.NewStore(t)
			srv, _ := receiver(t, code)
			counted := metrictest.Delta(t, "themis_notifications_total", map[string]string{"result": result})
			_ = (&notify.Worker{Store: s, Config: config.Notifications{Webhook: &config.Webhook{URL: srv.URL}}}).Notify(t.Context(), escalated(t, s))
			if got := counted(); got != 1 {
				t.Errorf("counted %v, want 1", got)
			}
		})
	}
}

// Without a webhook there is nothing to count.
func TestMetrics_NoWebhookCountsNothing(t *testing.T) {
	s, _ := pgtest.NewStore(t)
	delivered := metrictest.Delta(t, "themis_notifications_total", map[string]string{"result": "delivered"})
	_ = (&notify.Worker{Store: s}).Notify(t.Context(), escalated(t, s))
	if got := delivered(); got != 0 {
		t.Errorf("counted %v, want 0", got)
	}
}
