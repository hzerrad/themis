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
)

// Route is the method and path the handler is registered at, in the pattern
// syntax of http.ServeMux. The mux answers 405 for other methods.
const Route = "POST /v1/intake/alertmanager"

// MaxBodyBytes bounds a webhook request body. An alert is 1 to 2 KB, so this
// fits an alert storm of thousands while bounding memory; Alertmanager's
// max_alerts is unlimited by default, so the bound is ours.
const MaxBodyBytes = 4 << 20

// Receiver stores translated alerts. *intake.Receiver is the implementation.
type Receiver interface {
	Receive(ctx context.Context, alerts []intake.Alert, invalid []intake.InvalidAlert) (intake.Received, error)
}

// Handler accepts Alertmanager webhook requests. It only deals with HTTP: it
// authenticates, bounds and parses the request, then hands the alerts
// to the receiver, which stores them for processing.
//
// Responses: 202 when the alerts are stored; 400 for a malformed payload or an
// unsupported version; 401 for a missing or wrong token; 413 for a body over
// MaxBodyBytes; 503 when Themis cannot store the alerts or read its own token,
// so that Alertmanager retries.
type Handler struct {
	Receiver Receiver

	// TokenFile holds the token Alertmanager must send as a bearer token. It
	// is read on every request, so a rotated Secret takes effect without a
	// restart.
	TokenFile string

	// Logger defaults to slog.Default.
	Logger *slog.Logger

	// Now defaults to time.Now. It sets the alerts' ReceivedAt.
	Now func() time.Time
}

type receivedResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Logger
	if log == nil {
		log = slog.Default()
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	receivedAt := now()

	// Authenticate before reading the body, so unauthenticated senders cannot
	// make Themis read up to the size bound.
	want, err := h.token()
	if err != nil {
		log.Error("cannot read the intake token", "file", h.TokenFile, "error", err)
		http.Error(w, "intake temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !bearerMatches(r.Header.Get("Authorization"), want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
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
		log.Warn("refused an Alertmanager payload", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.TruncatedAlerts > 0 {
		log.Warn("Alertmanager dropped alerts from a notification; raise max_alerts on the Themis receiver",
			"truncated", payload.TruncatedAlerts, "receiver", payload.Receiver)
	}

	alerts, invalid, err := payload.toAlerts(receivedAt)
	if err != nil {
		log.Error("cannot translate an Alertmanager payload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, a := range invalid {
		log.Warn("rejected an invalid alert", "alert", a.Name, "error", a.Err)
	}

	got, err := h.Receiver.Receive(r.Context(), alerts, invalid)
	if err != nil {
		log.Error("cannot store received alerts", "alerts", len(alerts)+len(invalid), "error", err)
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

// token reads the expected token. An empty token is an error: it would let an
// empty bearer token through.
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

// bearerMatches reports whether an Authorization header carries the token. The
// comparison takes the same time wherever the first difference is, so response
// times do not reveal the token.
func bearerMatches(header string, want []byte) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) == 1
}
