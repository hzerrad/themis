package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/incident/suggestion"
	"github.com/hzerrad/themis/internal/platform/telemetry"
	"github.com/hzerrad/themis/internal/server/identity"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Suggestions takes a fix suggested by a person, and judges it exactly as it
// judges the investigator's.
type Suggestions interface {
	Submit(ctx context.Context, from suggestion.Suggester, s suggestion.Suggestion) (store.Proposal, error)
}

// suggest takes the same request as the gRPC API, in its JSON form.
func (a *API) suggest(w http.ResponseWriter, r *http.Request) {
	if a.Suggestions == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req themisv1.SubmitProposalRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		http.Error(w, "The request isn't valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.IncidentId = r.PathValue("id")
	person := ""
	if who, _ := identity.From(r.Context()); !who.Token {
		person = who.Username
	}
	stored, err := a.Suggestions.Submit(r.Context(), suggestion.Person(person), suggestion.Request(&req))
	var refused *suggestion.Error
	switch {
	case errors.As(err, &refused):
		http.Error(w, refused.Message, suggestionStatuses[refused.Kind])
		return
	case err != nil && r.Context().Err() != nil:
		http.Error(w, r.Context().Err().Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		a.logger().ErrorContext(r.Context(), "a suggestion failed", telemetry.FieldError, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data, err := protojson.Marshal(suggestion.Response(stored))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// suggestionStatuses say in HTTP why a suggestion wasn't kept.
var suggestionStatuses = map[suggestion.Kind]int{
	suggestion.Invalid:         http.StatusBadRequest,
	suggestion.Unauthenticated: http.StatusUnauthorized,
	suggestion.Forbidden:       http.StatusForbidden,
	suggestion.NotFound:        http.StatusNotFound,
	suggestion.TooMany:         http.StatusTooManyRequests,
	suggestion.Gone:            http.StatusConflict,
}
