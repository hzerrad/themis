package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func (s *Server) LeaseInvestigation(ctx context.Context, req *themisv1.LeaseInvestigationRequest) (*themisv1.LeaseInvestigationResponse, error) {
	now := s.now()
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
			if err := s.escalate(ctx, tx, id, "The investigator stopped responding during its last attempt."); err != nil {
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
		Budget: &themisv1.Budget{
			MaxSteps: int32(budget.MaxSteps), //nolint:gosec // validated config
			Deadline: timestamppb.New(now.Add(budget.Timeout.Duration)),
		},
	}
	for _, it := range items {
		out.InitialEvidence = append(out.InitialEvidence, itemToProto(it))
	}
	for _, r := range results {
		out.Predicates = append(out.Predicates, predicateToProto(r))
	}
	for _, name := range profile.Evidence.Allowed {
		t := &themisv1.EvidenceTemplate{Name: name}
		if d, ok := templates[name]; ok {
			t.Description, t.Params = d.GetDescription(), d.GetParams()
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

func (s *Server) CompleteInvestigation(ctx context.Context, req *themisv1.CompleteInvestigationRequest) (*themisv1.CompleteInvestigationResponse, error) {
	conclusion, ok := map[themisv1.InvestigationConclusion]string{
		themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED:     "proposed",
		themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE: "inconclusive",
	}[req.GetConclusion()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "conclusion must be PROPOSED or INCONCLUSIVE")
	}
	result, err := investigationResult(conclusion, req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "encode result: %v", err)
	}

	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		if err := s.Store.CompleteInvestigation(ctx, tx, inv.LeaseID, result); err != nil {
			return err
		}
		if conclusion == "inconclusive" {
			store.CountInvestigation(tx, store.InvestigationAbstained)
			return s.advance(ctx, tx, inv.IncidentID, req.GetSummary(), incident.Investigating, incident.Inconclusive, incident.ClosedNoAction)
		}

		store.CountInvestigation(tx, store.InvestigationProposed)
		proposals, err := s.Store.Proposals(ctx, tx, inv.IncidentID)
		if err != nil {
			return err
		}
		for _, p := range proposals {
			if p.LeaseID == inv.LeaseID && p.Status == store.ProposalAccepted {
				if err := s.advance(ctx, tx, inv.IncidentID, req.GetSummary(), incident.Investigating, incident.Diagnosed); err != nil {
					return err
				}
				return s.Store.EnqueueTx(ctx, tx, governance.GovernArgs{ProposalID: p.ID, IncidentID: inv.IncidentID})
			}
		}
		return s.advance(ctx, tx, inv.IncidentID, "The investigation suggested no fix that Themis could accept.", incident.Investigating, incident.Diagnosed, incident.ClosedNoAction)
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.CompleteInvestigationResponse{}, nil
}

func investigationResult(conclusion string, req *themisv1.CompleteInvestigationRequest) (store.InvestigationResult, error) {
	hypotheses := make([]json.RawMessage, 0, len(req.GetHypotheses()))
	for _, h := range req.GetHypotheses() {
		b, err := protojson.Marshal(h)
		if err != nil {
			return store.InvestigationResult{}, err
		}
		hypotheses = append(hypotheses, b)
	}
	hs, err := json.Marshal(hypotheses)
	if err != nil {
		return store.InvestigationResult{}, err
	}
	usage, err := protojson.Marshal(req.GetUsage())
	if err != nil {
		return store.InvestigationResult{}, err
	}
	provenance, err := protojson.Marshal(req.GetProvenance())
	if err != nil {
		return store.InvestigationResult{}, err
	}
	return store.InvestigationResult{Conclusion: conclusion, Summary: req.GetSummary(), Hypotheses: hs, Usage: usage, Provenance: provenance}, nil
}

func (s *Server) FailInvestigation(ctx context.Context, req *themisv1.FailInvestigationRequest) (*themisv1.FailInvestigationResponse, error) {
	err := s.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		retryAt := s.now().Add(retryBackoff << (inv.Attempts - 1))
		exhausted, err := s.Store.FailInvestigation(ctx, tx, inv.LeaseID, failed(req.GetReason()), req.GetMessage(), retryAt)
		if err != nil {
			return err
		}
		if budgetReasons[req.GetReason()] {
			store.CountInvestigation(tx, store.InvestigationBudgetExhausted)
		} else {
			store.CountInvestigation(tx, store.InvestigationFailed)
		}
		if !exhausted {
			return nil
		}
		return s.escalate(ctx, tx, inv.IncidentID, fmt.Sprintf("The investigation failed %d times. The last time, %s", store.MaxInvestigationAttempts, failures[req.GetReason()]))
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}
	return &themisv1.FailInvestigationResponse{}, nil
}

// failures say why an investigation failed, as the end of a sentence.
var failures = map[themisv1.InvestigationFailureReason]string{
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_UNSPECIFIED:          "the investigator gave no reason.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE:    "the model couldn't be reached.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT: "the model's answers weren't in the required format.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:     "it used up its token budget.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED:    "it ran out of time.",
	themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INTERNAL:             "the investigator hit an internal error.",
}

// failed says why an investigation failed, as a sentence of its own.
func failed(r themisv1.InvestigationFailureReason) string {
	why := strings.TrimSuffix(failures[r], ".")
	return "The investigation failed because " + why
}

func (s *Server) escalate(ctx context.Context, tx pgx.Tx, incidentID, reason string) error {
	err := s.advance(ctx, tx, incidentID, reason, incident.Investigating, incident.Escalated)
	if errors.Is(err, store.ErrStateConflict) {
		return nil
	}
	return err
}

// advance moves an incident along path, from path[0].
func (s *Server) advance(ctx context.Context, tx pgx.Tx, incidentID, reason string, path ...incident.State) error {
	for i := 1; i < len(path); i++ {
		err := s.Store.Transition(ctx, tx, store.TransitionArgs{ID: incidentID, From: path[i-1], To: path[i], Reason: reason, Actor: actor})
		if err != nil {
			return err
		}
	}
	return nil
}
