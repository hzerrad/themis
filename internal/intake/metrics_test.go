package intake_test

import (
	"errors"
	"testing"

	"github.com/riverqueue/river"

	"github.com/hzerrad/themis/internal/intake"
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

func TestMetrics_CreatedJoinedResolved(t *testing.T) {
	e := newWorkerEnv(t, nil)
	_, pod1 := k8s.NewRollout(t, e.ns, "checkout")
	pod2 := k8s.NewPod(t, e.ns, "checkout-2", pod1.OwnerReferences[0])
	created := metrictest.Delta(t, processed, outcome("created", ""))
	joined := metrictest.Delta(t, processed, outcome("joined", ""))
	resolved := metrictest.Delta(t, processed, outcome("resolved", ""))

	wantCompleted(t, e.process(t, alertFor(e.ns, pod1.Name, "fp-a", intake.AlertFiring), nil))
	wantCompleted(t, e.process(t, alertFor(e.ns, pod2.Name, "fp-b", intake.AlertFiring), nil))
	wantCompleted(t, e.process(t, alertFor(e.ns, pod1.Name, "fp-a", intake.AlertResolved), nil))

	wantDelta(t, "created", created, 1)
	wantDelta(t, "joined", joined, 1)
	wantDelta(t, "resolved", resolved, 1)
}

// Rejections are counted by reason: the raw signal of the coverage report.
func TestMetrics_RejectedByReason(t *testing.T) {
	e := newWorkerEnv(t, nil)
	notEnrolled := metrictest.Delta(t, processed, outcome("rejected", "not_enrolled"))

	wantCompleted(t, e.process(t, alertFor("kube-system", "coredns-1", "fp", intake.AlertFiring), nil))

	wantDelta(t, "rejected{not_enrolled}", notEnrolled, 1)
}

// A transient failure is a retry, and its last attempt a rejection.
func TestMetrics_RetriedThenProcessingFailed(t *testing.T) {
	e := newWorkerEnv(t, failingReader{errors.New("connection refused")})
	retried := metrictest.Delta(t, processed, outcome("retried", ""))
	failed := metrictest.Delta(t, processed, outcome("rejected", "processing_failed"))

	e.process(t, alertFor(e.ns, "x", "fp-1", intake.AlertFiring), &river.InsertOpts{MaxAttempts: 2})
	wantDelta(t, "retried", retried, 1)

	wantCompleted(t, e.process(t, alertFor(e.ns, "x", "fp-2", intake.AlertFiring), &river.InsertOpts{MaxAttempts: 1}))
	wantDelta(t, "rejected{processing_failed}", failed, 1)
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
