package httpapi

import (
	"context"
	"io"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hzerrad/themis/internal/server/identity"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Suggestions takes a fix suggested by a person. The core checks it exactly
// as it checks the investigator's.
type Suggestions interface {
	// SubmitFromPerson takes a suggestion from person, or from the holder of
	// the admin token when person is empty.
	SubmitFromPerson(ctx context.Context, person string, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error)
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
	resp, err := a.Suggestions.SubmitFromPerson(r.Context(), person, &req)
	if err != nil {
		st := status.Convert(err)
		http.Error(w, st.Message(), suggestionStatuses[st.Code()])
		return
	}
	data, err := protojson.Marshal(resp)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// suggestionStatuses map what the core says about a suggestion to HTTP.
var suggestionStatuses = map[codes.Code]int{
	codes.InvalidArgument:    http.StatusBadRequest,
	codes.PermissionDenied:   http.StatusForbidden,
	codes.NotFound:           http.StatusNotFound,
	codes.ResourceExhausted:  http.StatusTooManyRequests,
	codes.FailedPrecondition: http.StatusConflict,
	codes.Internal:           http.StatusInternalServerError,
	codes.Unavailable:        http.StatusServiceUnavailable,
	codes.Canceled:           http.StatusServiceUnavailable,
	codes.DeadlineExceeded:   http.StatusServiceUnavailable,
}
