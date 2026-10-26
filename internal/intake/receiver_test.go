package intake_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/intake"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

// source is the Source of the alerts in these tests.
const source = "alertmanager"

var receivedAt = time.Date(2026, 10, 26, 9, 12, 5, 0, time.UTC)

func validAlert(pod string) intake.Alert {
	return intake.Alert{
		Source:     source,
		SourceID:   "fp-" + pod,
		Name:       "KubePodCrashLooping",
		Status:     intake.AlertFiring,
		Labels:     map[string]string{"alertname": "KubePodCrashLooping", "namespace": "shop", "pod": pod},
		StartsAt:   time.Date(2026, 10, 26, 9, 12, 4, 512_000_000, time.UTC),
		ReceivedAt: receivedAt,
		Payload:    json.RawMessage(fmt.Sprintf(`{"status":"firing","labels":{"pod":%q}}`, pod)),
	}
}

func invalidAlert(payload string) intake.InvalidAlert {
	return intake.InvalidAlert{
		Source:     source,
		Labels:     map[string]string{"namespace": "shop", "pod": "b"},
		Err:        fmt.Errorf("%w: alert 1: %w", intake.ErrInvalidAlert, intake.ErrMissingAlertName),
		ReceivedAt: receivedAt,
		Payload:    json.RawMessage(payload),
	}
}

func TestReceive_QueuesValidAndRecordsInvalid(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	r := intake.NewReceiver(s)

	got, err := r.Receive(t.Context(),
		[]intake.Alert{validAlert("a"), validAlert("c")},
		[]intake.InvalidAlert{invalidAlert(`{"status":"firing","labels":{"pod":"b"}}`)},
	)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	if got != (intake.Received{Accepted: 2, Rejected: 1}) {
		t.Errorf("Received = %+v, want 2 accepted, 1 rejected", got)
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM river_job WHERE kind = 'process_alert' AND max_attempts = 10`); n != 2 {
		t.Errorf("process_alert jobs with max_attempts 10 = %d, want 2", n)
	}

	var reason, detail, alertName, namespace string
	var gotReceived time.Time
	err = pool.QueryRow(t.Context(),
		`SELECT reason::text, detail, alert_name, namespace, received_at FROM rejected_alerts`,
	).Scan(&reason, &detail, &alertName, &namespace, &gotReceived)
	if err != nil {
		t.Fatalf("read rejection: %v", err)
	}
	if reason != "invalid_alert" || alertName != "" || namespace != "shop" || !gotReceived.Equal(receivedAt) {
		t.Errorf("rejection = %q, name %q, namespace %q, received %v", reason, alertName, namespace, gotReceived)
	}
	if !strings.Contains(detail, "alert 1") || !strings.Contains(detail, "no name") {
		t.Errorf("detail = %q, want the translation error", detail)
	}
}

// The job carries the whole alert: processing gets exactly what was received.
func TestReceive_JobCarriesTheAlert(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	want := validAlert("a")

	if _, err := intake.NewReceiver(s).Receive(t.Context(), []intake.Alert{want}, nil); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT args FROM river_job`).Scan(&raw); err != nil {
		t.Fatalf("read job: %v", err)
	}
	var args intake.ProcessAlertArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("decode job args: %v", err)
	}
	got := args.Alert
	if got.Source != want.Source || got.SourceID != want.SourceID || got.Name != want.Name ||
		got.Status != want.Status || got.Labels["pod"] != "a" ||
		!got.StartsAt.Equal(want.StartsAt) || !got.ReceivedAt.Equal(want.ReceivedAt) {
		t.Errorf("job alert = %+v, want %+v", got, want)
	}
	var payload struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(got.Payload, &payload); err != nil || payload.Labels["pod"] != "a" {
		t.Errorf("job payload = %s (%v), want the alert's JSON", got.Payload, err)
	}
}

// When any part fails, nothing from the request is stored: the sender's retry
// must not find half of it already there.
func TestReceive_AllOrNothing(t *testing.T) {
	s, pool := pgtest.NewStore(t)

	// A payload that is not JSON makes the rejection insert fail after the
	// jobs were enqueued in the same transaction.
	_, err := intake.NewReceiver(s).Receive(t.Context(),
		[]intake.Alert{validAlert("a"), validAlert("c")},
		[]intake.InvalidAlert{invalidAlert(`{not json`)},
	)

	if err == nil {
		t.Fatal("Receive succeeded, want an error")
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
		t.Errorf("jobs = %d after a failed Receive, want 0", n)
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM rejected_alerts`); n != 0 {
		t.Errorf("rejections = %d after a failed Receive, want 0", n)
	}
}

func TestReceive_Nothing(t *testing.T) {
	s, pool := pgtest.NewStore(t)

	got, err := intake.NewReceiver(s).Receive(t.Context(), nil, nil)

	if err != nil || got != (intake.Received{}) {
		t.Errorf("Receive(nil, nil) = %+v, %v; want nothing", got, err)
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM river_job`); n != 0 {
		t.Errorf("jobs = %d, want 0", n)
	}
}

// An invalid alert without an error still gets a readable detail.
func TestReceive_InvalidAlertWithoutError(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	a := invalidAlert(`{}`)
	a.Err = nil

	if _, err := intake.NewReceiver(s).Receive(t.Context(), nil, []intake.InvalidAlert{a}); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM rejected_alerts WHERE detail <> ''`); n != 1 {
		t.Errorf("rejections with a detail = %d, want 1", n)
	}
}
