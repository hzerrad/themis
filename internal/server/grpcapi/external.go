package grpcapi

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

const (
	// Audience is who a caller's token must be issued for, so a token meant
	// for another service can't be replayed here.
	Audience = "themis"

	// PeerAttemptsPerMinute bounds, per address, calls with a token not
	// recently accepted, before Themis asks the API server about it. A caller
	// with a valid token needs about one a minute.
	PeerAttemptsPerMinute = 30
	// ReadsPerMinute bounds each source's ListIncidents and GetIncident calls.
	ReadsPerMinute = 120

	maxOpenIncidents = 100
)

// ListIncidents returns the open incidents the caller may suggest fixes for.
func (s *Server) ListIncidents(ctx context.Context, _ *themisv1.ListIncidentsRequest) (*themisv1.ListIncidentsResponse, error) {
	caller, err := s.reader(ctx)
	if err != nil {
		return nil, err
	}
	incidents, err := s.Store.OpenIncidents(ctx, caller.Namespaces, maxOpenIncidents)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.ListIncidentsResponse{}
	for _, in := range incidents {
		resp.Incidents = append(resp.Incidents, openIncident(in))
	}
	return resp, nil
}

// GetIncident returns what Themis knows about an incident in the caller's
// scope: redacted evidence up to its ceiling, and the checks.
func (s *Server) GetIncident(ctx context.Context, req *themisv1.GetIncidentRequest) (*themisv1.GetIncidentResponse, error) {
	caller, err := s.reader(ctx)
	if err != nil {
		return nil, err
	}
	in, err := s.Store.Incident(ctx, req.GetIncidentId())
	// One answer for both, so a caller can't learn of an incident out of its scope.
	if errors.Is(err, store.ErrNotFound) || (err == nil && !caller.InScope(in.Target.Namespace)) {
		return nil, status.Error(codes.NotFound, "no such incident")
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	items, err := s.Store.Evidence(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	results, err := s.Store.Predicates(ctx, in.ID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	resp := &themisv1.GetIncidentResponse{Incident: openIncident(in)}
	for _, it := range items {
		if it.Classification == evidence.ClassificationInternal || caller.Ceiling == evidence.ClassificationSensitive {
			resp.Evidence = append(resp.Evidence, itemToProto(it))
		} else {
			resp.Withheld++
		}
	}
	for _, r := range results {
		resp.Predicates = append(resp.Predicates, s.predicateToProto(r))
	}
	if profile, ok := s.Config.Current().Profile(in.Profile); ok {
		var allowed []string
		for _, c := range profile.Capabilities {
			if slices.Contains(caller.Fixes, c) {
				allowed = append(allowed, c)
			}
		}
		resp.Capabilities = s.capabilities(allowed)
	}
	return resp, nil
}
