package alertmanager_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/intake/alertmanager"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "alerts", name)) //nolint:gosec // a fixture name chosen by the test
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// A payload as kube-prometheus-stack's Alertmanager sends it: one group with a
// firing and a resolved alert.
func TestParseAlertmanagerPayload_Fixture(t *testing.T) {
	p, err := alertmanager.ParsePayload(readFixture(t, "kube-pod-crashlooping.json"))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}

	if p.Version != alertmanager.PayloadVersion {
		t.Errorf("Version = %q, want %q", p.Version, alertmanager.PayloadVersion)
	}
	if p.TruncatedAlerts != 0 {
		t.Errorf("TruncatedAlerts = %d, want 0", p.TruncatedAlerts)
	}
	if len(p.Alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(p.Alerts))
	}

	firing, resolved := p.Alerts[0], p.Alerts[1]
	if firing.Status != "firing" || resolved.Status != "resolved" {
		t.Errorf("statuses = %q, %q, want firing, resolved", firing.Status, resolved.Status)
	}
	if got := firing.Labels["alertname"]; got != "KubePodCrashLooping" {
		t.Errorf("alertname = %q, want KubePodCrashLooping", got)
	}
	if got := firing.Labels["pod"]; got != "checkout-7d9f8c6b5-x2x7q" {
		t.Errorf("pod = %q, want checkout-7d9f8c6b5-x2x7q", got)
	}
	wantStart := time.Date(2026, 10, 26, 9, 12, 4, 512_000_000, time.UTC)
	if !firing.StartsAt.Equal(wantStart) {
		t.Errorf("StartsAt = %v, want %v", firing.StartsAt, wantStart)
	}
	// Alertmanager sends the zero time as endsAt for a firing alert.
	if !firing.EndsAt.IsZero() {
		t.Errorf("firing EndsAt = %v, want the zero time", firing.EndsAt)
	}
	if resolved.EndsAt.IsZero() {
		t.Error("resolved EndsAt is the zero time, want when it ended")
	}
}

// The JSON key is camelCase; a mistyped tag would silently decode to 0.
func TestParseAlertmanagerPayload_TruncatedAlerts(t *testing.T) {
	p, err := alertmanager.ParsePayload([]byte(`{"version":"4","alerts":[],"truncatedAlerts":3}`))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	if p.TruncatedAlerts != 3 {
		t.Errorf("TruncatedAlerts = %d, want 3", p.TruncatedAlerts)
	}
}

func TestParseAlertmanagerPayload_Errors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{"other version", `{"version":"5","alerts":[]}`, alertmanager.ErrUnsupportedVersion},
		{"missing version", `{"alerts":[]}`, alertmanager.ErrUnsupportedVersion},
		{"version as a number", `{"version":4,"alerts":[]}`, alertmanager.ErrParseFailed},
		{"empty body", ``, alertmanager.ErrParseFailed},
		{"not JSON", `version=4`, alertmanager.ErrParseFailed},
		{"not an object", `[{"version":"4"}]`, alertmanager.ErrParseFailed},
		{"wrong field type", `{"version":"4","alerts":"none"}`, alertmanager.ErrParseFailed},
		{"duplicate key", `{"version":"4","version":"4","alerts":[]}`, alertmanager.ErrParseFailed},
		{"trailing data", `{"version":"4","alerts":[]} {}`, alertmanager.ErrParseFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := alertmanager.ParsePayload([]byte(tt.body))

			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if p != nil {
				t.Errorf("payload = %+v, want nil", p)
			}
		})
	}
}

// The version error names the version received, so an operator can tell
// which Alertmanager sent it.
func TestParseAlertmanagerPayload_VersionErrorNamesVersion(t *testing.T) {
	_, err := alertmanager.ParsePayload([]byte(`{"version":"5","alerts":[]}`))

	if err == nil || !strings.Contains(err.Error(), `"5"`) {
		t.Errorf("err = %v, want it to name version \"5\"", err)
	}
}

// The parse error keeps the decoder's cause, so logs show what was wrong.
func TestParseAlertmanagerPayload_ParseErrorKeepsCause(t *testing.T) {
	_, err := alertmanager.ParsePayload([]byte(`{"version":"4","version":"4"}`))

	if err == nil || err.Error() == alertmanager.ErrParseFailed.Error() {
		t.Errorf("err = %v, want the sentinel plus the decoder's cause", err)
	}
}
