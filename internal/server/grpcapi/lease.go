package grpcapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) LeaseInvestigation(ctx context.Context, req *themisv1.LeaseInvestigationRequest) (*themisv1.LeaseInvestigationResponse, error) {
	now := s.now()
	s.Heartbeat.Beat(ctx, req.GetWorkerId())
	var (
		inv store.Investigation
		ok  bool
	)
	err := s.Store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := s.Store.ExpireInvestigations(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, id := range expired {
			if err := s.abandon(ctx, tx, id, "The investigator stopped responding during its last attempt."); err != nil {
				return err
			}
		}
		inv, ok, err = s.Store.LeaseInvestigation(ctx, tx, req.GetWorkerId(), now, s.leaseFor())
		return err
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	if !ok {
		return &themisv1.LeaseInvestigationResponse{}, nil
	}

	profile, err := s.profile(inv)
	if err != nil {
		return nil, err
	}
	items, err := s.Store.Evidence(ctx, inv.IncidentID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	results, err := s.Store.Predicates(ctx, inv.IncidentID)
	if err != nil {
		return nil, s.internal(ctx, err)
	}

	budget := profile.InvestigationBudget()
	out := &themisv1.Investigation{
		LeaseId:               inv.LeaseID,
		LeaseDeadline:         timestamppb.New(inv.LeaseDeadline),
		IncidentId:            inv.IncidentID,
		Profile:               inv.Profile,
		Target:                targetToProto(inv.Target),
		CandidateCapabilities: profile.Capabilities,
		Capabilities:          s.capabilities(profile.Capabilities),
		Budget: &themisv1.Budget{
			MaxSteps: int32(budget.MaxSteps), //nolint:gosec // validated config
			Deadline: timestamppb.New(now.Add(budget.Timeout.Duration)),
		},
	}
	for _, it := range items {
		out.InitialEvidence = append(out.InitialEvidence, itemToProto(it))
	}
	for _, r := range results {
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
	var deadline *timestamppb.Timestamp
	err := s.Store.InTx(ctx, func(tx pgx.Tx) error {
		d, err := s.Store.RenewLease(ctx, tx, req.GetLeaseId(), s.now(), s.leaseFor())
		deadline = timestamppb.New(d)
		return err
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.RenewLeaseResponse{LeaseDeadline: deadline}, nil
}

func (s *Server) leaseFor() time.Duration {
	if s.LeaseFor > 0 {
		return s.LeaseFor
	}
	return DefaultLeaseFor
}
