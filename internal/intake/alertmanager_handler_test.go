package intake_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/intake"
	"github.com/hzerrad/themis/internal/testutil/pgtest"
)

const token = "s3cret-token"

// fakeReceiver records what the handler hands over.
type fakeReceiver struct {
	calls   int
	alerts  []intake.Alert
	invalid []intake.InvalidAlert
	err     error
}

func (f *fakeReceiver) Receive(_ context.Context, alerts []intake.Alert, invalid []intake.InvalidAlert) (intake.Received, error) {
	f.calls++
	f.alerts, f.invalid = alerts, invalid
	if f.err != nil {
		return intake.Received{}, f.err
	}
	return intake.Received{Accepted: len(alerts), Rejected: len(invalid)}, nil
}

func writeToken(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func newHandler(t *testing.T, r intake.AlertReceiver) (*intake.AlertmanagerHandler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return &intake.AlertmanagerHandler{
		Receiver:  r,
		TokenFile: writeToken(t, token+"\n"),
		Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		Now:       func() time.Time { return receivedAt },
	}, &logs
}

func post(h http.Handler, auth string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/intake/alertmanager", bytes.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func fixture(t *testing.T) []byte {
	t.Helper()
	return readFixture(t, "kube-pod-crashlooping.json")
}

func TestAlertmanagerHandler_Accepts(t *testing.T) {
	r := &fakeReceiver{}
	h, _ := newHandler(t, r)

	rec := post(h, "Bearer "+token, fixture(t))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body)
	}
	var resp struct{ Accepted, Rejected int }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Accepted != 2 || resp.Rejected != 0 {
		t.Errorf("body = %s, want 2 accepted, 0 rejected", rec.Body)
	}
	if len(r.alerts) != 2 {
		t.Fatalf("receiver got %d alerts, want 2", len(r.alerts))
	}
	if !r.alerts[0].ReceivedAt.Equal(receivedAt) {
		t.Errorf("ReceivedAt = %v, want the handler's clock %v", r.alerts[0].ReceivedAt, receivedAt)
	}
}

func TestAlertmanagerHandler_Unauthorized(t *testing.T) {
	tests := map[string]string{
		"no header":    "",
		"wrong token":  "Bearer not-the-token",
		"wrong scheme": "Basic " + token,
		"no token":     "Bearer ",
		"token prefix": "Bearer " + token[:4],
	}
	for name, auth := range tests {
		t.Run(name, func(t *testing.T) {
			r := &fakeReceiver{}
			h, _ := newHandler(t, r)

			rec := post(h, auth, fixture(t))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if r.calls != 0 {
				t.Error("receiver called for an unauthorized request")
			}
		})
	}
}

// The scheme is case-insensitive (RFC 7235).
func TestAlertmanagerHandler_LowercaseScheme(t *testing.T) {
	h, _ := newHandler(t, &fakeReceiver{})

	if rec := post(h, "bearer "+token, fixture(t)); rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

// A rotated Secret takes effect on the next request, without a restart.
func TestAlertmanagerHandler_TokenRotation(t *testing.T) {
	h, _ := newHandler(t, &fakeReceiver{})
	if err := os.WriteFile(h.TokenFile, []byte("rotated-token"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}

	if rec := post(h, "Bearer "+token, fixture(t)); rec.Code != http.StatusUnauthorized {
		t.Errorf("old token: status = %d, want 401", rec.Code)
	}
	if rec := post(h, "Bearer rotated-token", fixture(t)); rec.Code != http.StatusAccepted {
		t.Errorf("new token: status = %d, want 202", rec.Code)
	}
}

// Themis's own misconfiguration is a 503, so Alertmanager retries; an empty
// token file never lets an empty bearer token through.
func TestAlertmanagerHandler_TokenFileProblems(t *testing.T) {
	tests := map[string]func(h *intake.AlertmanagerHandler){
		"missing file": func(h *intake.AlertmanagerHandler) { h.TokenFile = filepath.Join(t.TempDir(), "absent") },
		"empty file":   func(h *intake.AlertmanagerHandler) { h.TokenFile = writeToken(t, "\n") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := &fakeReceiver{}
			h, logs := newHandler(t, r)
			mutate(h)

			rec := post(h, "Bearer ", fixture(t))

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
			if r.calls != 0 {
				t.Error("receiver called")
			}
			if !strings.Contains(logs.String(), "cannot read the intake token") {
				t.Errorf("logs = %q, want the token error", logs)
			}
		})
	}
}

func TestAlertmanagerHandler_BadPayload(t *testing.T) {
	tests := map[string]string{
		"not JSON":    `version=4`,
		"old version": `{"version":"3","alerts":[]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			r := &fakeReceiver{}
			h, _ := newHandler(t, r)

			rec := post(h, "Bearer "+token, []byte(body))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if r.calls != 0 {
				t.Error("receiver called for a bad payload")
			}
		})
	}
}

func TestAlertmanagerHandler_TooLarge(t *testing.T) {
	r := &fakeReceiver{}
	h, _ := newHandler(t, r)
	body := append([]byte(`{"version":"4","alerts":[],"pad":"`), bytes.Repeat([]byte("x"), intake.MaxAlertmanagerBodyBytes)...)

	rec := post(h, "Bearer "+token, body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if r.calls != 0 {
		t.Error("receiver called for an oversized body")
	}
}

func TestAlertmanagerHandler_StoreFailureIs503(t *testing.T) {
	h, logs := newHandler(t, &fakeReceiver{err: errors.New("database down")})

	rec := post(h, "Bearer "+token, fixture(t))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(logs.String(), "database down") {
		t.Errorf("logs = %q, want the store error", logs)
	}
}

// One malformed alert does not cost the request: it is handed over as invalid.
func TestAlertmanagerHandler_InvalidAlertStillAccepted(t *testing.T) {
	r := &fakeReceiver{}
	h, _ := newHandler(t, r)
	body := []byte(`{"version":"4","alerts":[
		{"status":"firing","labels":{"alertname":"KubePodCrashLooping","namespace":"shop","pod":"a"}},
		{"status":"firing","labels":{"namespace":"shop","pod":"b"}}
	]}`)

	rec := post(h, "Bearer "+token, body)

	if rec.Code != http.StatusAccepted || len(r.alerts) != 1 || len(r.invalid) != 1 {
		t.Errorf("status %d, %d alerts, %d invalid; want 202, 1, 1", rec.Code, len(r.alerts), len(r.invalid))
	}
}

func TestAlertmanagerHandler_WarnsWhenTruncated(t *testing.T) {
	h, logs := newHandler(t, &fakeReceiver{})

	post(h, "Bearer "+token, []byte(`{"version":"4","alerts":[],"truncatedAlerts":3}`))

	if !strings.Contains(logs.String(), "truncated=3") {
		t.Errorf("logs = %q, want a warning with truncated=3", logs)
	}
}

// Registered at AlertmanagerRoute, other methods are refused by the mux.
func TestAlertmanagerHandler_RouteRefusesOtherMethods(t *testing.T) {
	h, _ := newHandler(t, &fakeReceiver{})
	mux := http.NewServeMux()
	mux.Handle(intake.AlertmanagerRoute, h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/intake/alertmanager", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

// End to end with the real receiver: the fixture's alerts become jobs.
func TestAlertmanagerHandler_StoresJobs(t *testing.T) {
	s, pool := pgtest.NewStore(t)
	h, _ := newHandler(t, intake.NewReceiver(s))

	if rec := post(h, "Bearer "+token, fixture(t)); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", rec.Code, rec.Body)
	}
	if n := pgtest.Count(t, pool, `SELECT count(*) FROM river_job WHERE kind = 'process_alert'`); n != 2 {
		t.Errorf("process_alert jobs = %d, want 2", n)
	}
}
