package intake_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/incident/intake"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const processed = "themis_intake_alerts_processed_total"

func outcome(o, reason string) map[string]string {
	return map[string]string{"outcome": o, "reason": reason}
}

func wantDelta(t *testing.T, what string, delta func() float64, want float64) {
	t.Helper()
	if got := delta(); got != want {
		t.Errorf("%s changed by %v, want %v", what, got, want)
	}
}

// Alerts are counted as received only once they are stored.
func TestMetrics_Received(t *testing.T) {
	const name = "themis_intake_alerts_received_total"
	accepted := metrictest.Delta(t, name, map[string]string{"source": source, "result": "accepted"})
	invalid := metrictest.Delta(t, name, map[string]string{"source": source, "result": "invalid"})
	s, _ := pgtest.NewStore(t)

	_, err := intake.NewReceiver(s).Receive(t.Context(),
		[]intake.Alert{validAlert("a"), validAlert("c")},
		[]intake.InvalidAlert{invalidAlert(`{}`)},
	)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	wantDelta(t, "accepted", accepted, 2)
	wantDelta(t, "invalid", invalid, 1)

	// A failed Receive stores nothing, so it counts nothing.
	_, _ = intake.NewReceiver(s).Receive(t.Context(),
		[]intake.Alert{validAlert("a")},
		[]intake.InvalidAlert{invalidAlert(`{not json`)},
	)
	wantDelta(t, "accepted after a failed Receive", accepted, 2)
}
