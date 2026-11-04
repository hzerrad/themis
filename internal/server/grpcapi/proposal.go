package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/incident/suggestion"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// SubmitProposal takes a suggestion from the investigator, under its lease,
// or from a caller outside Themis whose token proved who it is.
func (s *Server) SubmitProposal(ctx context.Context, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	from := suggestion.Investigator
	if caller, ok := callerFrom(ctx); ok {
		from = caller.Suggester
	}
	stored, err := s.Suggestions.Submit(ctx, from, suggestion.Request(req))
	var refused *suggestion.Error
	if errors.As(err, &refused) {
		return nil, status.Error(suggestionCodes[refused.Kind], refused.Message)
	}
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return suggestion.Response(stored), nil
}

// suggestionCodes say in gRPC why a suggestion wasn't kept.
var suggestionCodes = map[suggestion.Kind]codes.Code{
	suggestion.Invalid:         codes.InvalidArgument,
	suggestion.Unauthenticated: codes.Unauthenticated,
	suggestion.Forbidden:       codes.PermissionDenied,
	suggestion.NotFound:        codes.NotFound,
	suggestion.TooMany:         codes.ResourceExhausted,
	suggestion.Gone:            codes.FailedPrecondition,
}

// capabilities describes the fixes named that Themis can carry out, for a
// suggester.
func (s *Server) capabilities(names []string) []*themisv1.Capability {
	var out []*themisv1.Capability
	for _, name := range names {
		f, ok := s.Fixes[name]
		if !ok {
			continue
		}
		out = append(out, &themisv1.Capability{Name: f.Name(), Description: f.Description(), ParamsSchema: string(f.Schema())})
	}
	return out
}
