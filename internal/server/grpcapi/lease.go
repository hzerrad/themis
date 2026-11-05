package grpcapi

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) LeaseInvestigation(ctx context.Context, req *themisv1.LeaseInvestigationRequest) (*themisv1.LeaseInvestigationResponse, error) {
	s.Heartbeat.Beat(ctx, req.GetWorkerId())
	inv, ok, err := s.Investigations.Lease(ctx, req.GetWorkerId())
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	if !ok {
		return &themisv1.LeaseInvestigationResponse{}, nil
	}

	profile := inv.Scope
	out := &themisv1.Investigation{
		LeaseId:               inv.LeaseID,
		LeaseDeadline:         timestamppb.New(inv.LeaseDeadline),
		IncidentId:            inv.IncidentID,
		Profile:               inv.Profile,
		Target:                targetToProto(inv.Target),
		CandidateCapabilities: profile.Capabilities,
		Capabilities:          s.capabilities(profile.Capabilities),
		Guidance:              profile.Guidance,
		Budget: &themisv1.Budget{
			MaxSteps: int32(profile.InvestigationBudget().MaxSteps), //nolint:gosec // validated config
			Deadline: timestamppb.New(inv.Deadline),
		},
	}
	for _, it := range inv.Evidence {
		out.InitialEvidence = append(out.InitialEvidence, itemToProto(it))
	}
	for _, r := range inv.Results {
		out.Predicates = append(out.Predicates, s.predicateToProto(r))
	}
	for _, c := range s.RootCauses.For(inv.Profile) {
		out.RootCauses = append(out.RootCauses, &themisv1.RootCause{Name: c.Name, Description: c.Description})
	}
	for _, name := range profile.Evidence.Allowed {
		t := &themisv1.EvidenceTemplate{Name: name}
		if d, ok := s.Evidence[name]; ok {
			t.Description, t.Params = d.Description(), d.Params()
		}
		out.AllowedTemplates = append(out.AllowedTemplates, t)
	}
	return &themisv1.LeaseInvestigationResponse{Investigation: out}, nil
}

func (s *Server) RenewLease(ctx context.Context, req *themisv1.RenewLeaseRequest) (*themisv1.RenewLeaseResponse, error) {
	deadline, err := s.Investigations.Renew(ctx, req.GetLeaseId())
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.RenewLeaseResponse{LeaseDeadline: timestamppb.New(deadline)}, nil
}
