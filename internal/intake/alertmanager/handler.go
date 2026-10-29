package alertmanager

import (
	"context"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hzerrad/themis/internal/intake"
	"github.com/hzerrad/themis/internal/telemetry"
)

const Route = "POST /v1/intake/alertmanager"

// MaxBodyBytes fits thousands of alerts; Alertmanager's max_alerts is
// unbounded by default.
const MaxBodyBytes = 4 << 20

// Receiver stores translated alerts.
type Receiver interface {
	Receive(ctx context.Context, alerts []intake.Alert, invalid []intake.InvalidAlert) (intake.Received, error)
}

// Handler accepts Alertmanager webhooks: it authenticates, bounds and parses
// the request and hands the alerts to the receiver. 503 makes Alertmanager
// retry.
type Handler struct {
	Receiver Receiver

	// TokenFile is read on every request, so rotation needs no restart.
	TokenFile string

	Logger *slog.Logger
	Now    func() time.Time
}

type receivedResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w := &statusWriter{ResponseWriter: rw, code: http.StatusOK}
	defer func() { intake.ObserveRequest(Source, w.code) }()

	ctx := r.Context()
	log := h.Logger
	if log == nil {
		log = slog.Default()
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	receivedAt := now()

	// Authenticate before reading the body.
	want, err := h.token()
	if err != nil {
		log.ErrorContext(ctx, "cannot read the intake token", "file", h.TokenFile, telemetry.FieldError, err)
		http.Error(w, "intake temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !bearerMatches(r.Header.Get("Authorization"), want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// The original writer, so the server can close the connection.
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, MaxBodyBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request body", http.StatusBadRequest)
		return
	}

	payload, err := ParsePayload(body)
	if err != nil {
		log.WarnContext(ctx, "refused an Alertmanager payload", telemetry.FieldError, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.TruncatedAlerts > 0 {
		intake.ObserveTruncated(Source, payload.TruncatedAlerts)
		log.WarnContext(ctx, "Alertmanager dropped alerts from a notification; raise max_alerts on the Themis receiver",
			"truncated", payload.TruncatedAlerts, "receiver", payload.Receiver)
	}

	alerts, invalid, err := payload.toAlerts(receivedAt)
	if err != nil {
		log.ErrorContext(ctx, "cannot translate an Alertmanager payload", telemetry.FieldError, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, a := range invalid {
		log.WarnContext(ctx, "rejected an invalid alert", "alert", a.Name, telemetry.FieldError, a.Err)
	}

	got, err := h.Receiver.Receive(ctx, alerts, invalid)
	if err != nil {
		log.ErrorContext(ctx, "cannot store received alerts", "alerts", len(alerts)+len(invalid), telemetry.FieldError, err)
		http.Error(w, "intake temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	resp, err := json.Marshal(receivedResponse{Accepted: got.Accepted, Rejected: got.Rejected})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(resp)
}

// token reads the expected token; an empty one would let anyone in.
func (h *Handler) token() ([]byte, error) {
	data, err := os.ReadFile(h.TokenFile)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, errors.New("token file is empty")
	}
	return []byte(token), nil
}

// bearerMatches compares in constant time.
func bearerMatches(header string, want []byte) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) == 1
}

// statusWriter remembers the status code, for the requests metric.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
