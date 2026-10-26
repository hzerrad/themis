package intake

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Translation is internal to intake, so these tests are in package intake.

func parseFixture(t *testing.T, name string) *AlertmanagerPayload {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "alerts", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return mustParse(t, data)
}

func mustParse(t *testing.T, data []byte) *AlertmanagerPayload {
	t.Helper()
	p, err := ParseAlertmanagerPayload(data)
	if err != nil {
		t.Fatalf("ParseAlertmanagerPayload: %v", err)
	}
	return p
}

var received = time.Date(2026, 10, 26, 9, 12, 5, 0, time.UTC)

// Each alert in the group becomes its own Alert, built from that alert's own
// fields, not the group's.
func TestToAlerts_OnePerAlert(t *testing.T) {
	alerts, invalid, err := parseFixture(t, "kube-pod-crashlooping.json").toAlerts(received)
	if err != nil {
		t.Fatalf("toAlerts: %v", err)
	}
	if len(invalid) != 0 {
		t.Errorf("got %d invalid alerts, want 0: %v", len(invalid), invalid)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(alerts))
	}

	tests := []struct {
		name     string
		got      Alert
		sourceID string
		status   AlertStatus
		pod      string
		startsAt time.Time
		endsAt   time.Time
	}{
		{
			name:     "firing",
			got:      alerts[0],
			sourceID: "9c4f1d2b7a8e6c30",
			status:   AlertFiring,
			pod:      "checkout-7d9f8c6b5-x2x7q",
			startsAt: time.Date(2026, 10, 26, 9, 12, 4, 512_000_000, time.UTC),
			endsAt:   time.Time{},
		},
		{
			name:     "resolved",
			got:      alerts[1],
			sourceID: "e21a7b3c9d4f8016",
			status:   AlertResolved,
			pod:      "checkout-7d9f8c6b5-m4kzd",
			startsAt: time.Date(2026, 10, 26, 8, 41, 34, 512_000_000, time.UTC),
			endsAt:   time.Date(2026, 10, 26, 9, 6, 34, 512_000_000, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.got
			if a.Source != AlertmanagerSource {
				t.Errorf("Source = %q, want %q", a.Source, AlertmanagerSource)
			}
			if a.SourceID != tt.sourceID {
				t.Errorf("SourceID = %q, want %q", a.SourceID, tt.sourceID)
			}
			if a.Name != "KubePodCrashLooping" {
				t.Errorf("Name = %q, want KubePodCrashLooping", a.Name)
			}
			if a.Status != tt.status {
				t.Errorf("Status = %q, want %q", a.Status, tt.status)
			}
			// pod differs between the two alerts, so it is not in the group's
			// commonLabels: it must come from the alert itself.
			if got := a.Labels["pod"]; got != tt.pod {
				t.Errorf("Labels[pod] = %q, want %q", got, tt.pod)
			}
			if !a.StartsAt.Equal(tt.startsAt) {
				t.Errorf("StartsAt = %v, want %v", a.StartsAt, tt.startsAt)
			}
			if !a.EndsAt.Equal(tt.endsAt) {
				t.Errorf("EndsAt = %v, want %v", a.EndsAt, tt.endsAt)
			}
			if !a.ReceivedAt.Equal(received) {
				t.Errorf("ReceivedAt = %v, want %v", a.ReceivedAt, received)
			}
		})
	}
}

// Payload holds that one alert, not the whole group.
func TestToAlerts_PayloadIsTheSingleAlert(t *testing.T) {
	alerts, _, err := parseFixture(t, "kube-pod-crashlooping.json").toAlerts(received)
	if err != nil {
		t.Fatalf("toAlerts: %v", err)
	}

	for i, a := range alerts {
		var got struct {
			Status string            `json:"status"`
			Labels map[string]string `json:"labels"`
			Alerts []any             `json:"alerts"`
		}
		if err := json.Unmarshal(a.Payload, &got); err != nil {
			t.Fatalf("alert %d: Payload is not JSON: %v", i, err)
		}
		if got.Alerts != nil {
			t.Errorf("alert %d: Payload is the whole group, want the single alert", i)
		}
		if got.Status != string(a.Status) || got.Labels["pod"] != a.Labels["pod"] {
			t.Errorf("alert %d: Payload is another alert: status %q, pod %q", i, got.Status, got.Labels["pod"])
		}
	}
}

func TestToAlerts_NoAlerts(t *testing.T) {
	alerts, invalid, err := mustParse(t, []byte(`{"version":"4","alerts":[]}`)).toAlerts(received)

	if err != nil || len(alerts) != 0 || len(invalid) != 0 {
		t.Errorf("toAlerts = %v, %v, %v; want nothing", alerts, invalid, err)
	}
}

// A malformed alert is returned as invalid, and the valid alerts around it
// are still accepted.
func TestToAlerts_InvalidAlertDoesNotCostTheOthers(t *testing.T) {
	p := mustParse(t, []byte(`{"version":"4","alerts":[
		{"status":"firing","labels":{"alertname":"KubePodCrashLooping","namespace":"shop","pod":"a"},"fingerprint":"f0"},
		{"status":"firing","labels":{"namespace":"shop","pod":"b"},"fingerprint":"f1"},
		{"status":"firing","labels":{"alertname":"","pod":"c"},"fingerprint":"f2"},
		{"status":"pending","labels":{"alertname":"KubePodOOMKilled","pod":"d"},"fingerprint":"f3"},
		{"status":"resolved","labels":{"alertname":"KubePodOOMKilled","namespace":"shop","pod":"e"},"fingerprint":"f4"}
	]}`))

	alerts, invalid, err := p.toAlerts(received)
	if err != nil {
		t.Fatalf("toAlerts: %v", err)
	}

	if len(alerts) != 2 || alerts[0].SourceID != "f0" || alerts[1].SourceID != "f4" {
		t.Fatalf("valid alerts = %+v, want the alerts with fingerprints f0 and f4", alerts)
	}

	tests := []struct {
		index    int
		pod      string
		name     string
		reason   error
		inDetail string
	}{
		{1, "b", "", ErrMissingAlertName, "alert 1"},
		{2, "c", "", ErrMissingAlertName, "alert 2"},
		{3, "d", "KubePodOOMKilled", ErrUnsupportedAlertStatus, `"pending"`},
	}
	if len(invalid) != len(tests) {
		t.Fatalf("got %d invalid alerts, want %d: %v", len(invalid), len(tests), invalid)
	}
	for i, tt := range tests {
		got := invalid[i]
		if !errors.Is(got.Err, ErrInvalidAlert) || !errors.Is(got.Err, tt.reason) {
			t.Errorf("alert %d: Err = %v, want ErrInvalidAlert and %v", tt.index, got.Err, tt.reason)
		}
		if got.Err == nil || !strings.Contains(got.Err.Error(), tt.inDetail) {
			t.Errorf("alert %d: Err = %v, want it to mention %s", tt.index, got.Err, tt.inDetail)
		}
		if got.Name != tt.name || got.Labels["pod"] != tt.pod {
			t.Errorf("alert %d: Name %q, pod %q; want %q, %q", tt.index, got.Name, got.Labels["pod"], tt.name, tt.pod)
		}
		if got.Source != AlertmanagerSource || !got.ReceivedAt.Equal(received) || len(got.Payload) == 0 {
			t.Errorf("alert %d: Source %q, ReceivedAt %v, Payload %d bytes; want all set", tt.index, got.Source, got.ReceivedAt, len(got.Payload))
		}
	}
}

func TestAlertmanagerStatus(t *testing.T) {
	for _, s := range []AlertStatus{AlertFiring, AlertResolved} {
		if got, err := alertmanagerStatus(string(s)); err != nil || got != s {
			t.Errorf("alertmanagerStatus(%q) = %q, %v; want %q", s, got, err, s)
		}
	}
	for _, s := range []string{"", "pending", "FIRING"} {
		if _, err := alertmanagerStatus(s); !errors.Is(err, ErrUnsupportedAlertStatus) {
			t.Errorf("alertmanagerStatus(%q) error = %v, want ErrUnsupportedAlertStatus", s, err)
		}
	}
}

// The kind is stored with every queued job; renaming it strands them.
func TestProcessAlertArgs_KindIsPinned(t *testing.T) {
	if got := (ProcessAlertArgs{}).Kind(); got != "process_alert" {
		t.Errorf("Kind = %q, want process_alert", got)
	}
}

// River's default of 25 attempts would retry a stale alert for about 18 days.
func TestProcessAlertArgs_BoundedAttempts(t *testing.T) {
	if got := (ProcessAlertArgs{}).InsertOpts().MaxAttempts; got != 10 {
		t.Errorf("MaxAttempts = %d, want 10 (about 4 hours of retries)", got)
	}
}
