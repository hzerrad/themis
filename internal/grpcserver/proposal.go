package grpcserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/predicate"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// CapabilityRollback names the rollback_to_revision case of a proposal.
const (
	CapabilityRollback        = "deployment.rollback-to-revision"
	CapabilityRollbackVersion = "1"
)

var predicateNames = []string{
	predicate.NameRolloutRecent, predicate.NameFailureOnsetAfterRollout, predicate.NamePriorRevisionWasHealthy,
	predicate.NameOOMKilledNewRevision, predicate.NameGitOpsManaged, predicate.NameConfigChangedBeforeFailure,
}

// rejection is why a proposal is stored as rejected.
type rejection struct {
	reason  themisv1.RejectionReason
	message string
}

func (s *Server) SubmitProposal(ctx context.Context, req *themisv1.SubmitProposalRequest) (*themisv1.SubmitProposalResponse, error) {
	if caller, ok := callerFrom(ctx); ok {
		return s.submitExternal(ctx, caller, req)
	}
	if req.GetIncidentId() != "" {
		return nil, status.Error(codes.Unauthenticated, "only a caller outside Themis names an incident")
	}
	capability, params, invalid, err := parseProposal(req)
	if err != nil {
		return nil, err
	}

	var stored store.Proposal
	err = s.Store.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := s.Store.LeasedInvestigation(ctx, tx, req.GetLeaseId(), s.now())
		if err != nil {
			return err
		}
		profile, err := s.profile(inv)
		if err != nil {
			return err
		}
		p := store.Proposal{
			IncidentID: inv.IncidentID, LeaseID: inv.LeaseID, ClientProposalID: req.GetClientProposalId(),
			Capability: capability, CapabilityVersion: CapabilityRollbackVersion,
			Target: targetFromProto(req.GetTarget()), Params: params,
			ExpectedEffect: req.GetExpectedEffect(), Rationale: req.GetRationale(),
			CitedEvidenceIDs: req.GetCitedEvidenceIds(), CitedPredicates: req.GetCitedPredicates(),
			Provenance: provenance(req.GetProvenance()),
			// The lease proves it comes from the investigator.
			Authenticated: true,
			Status:        store.ProposalAccepted,
		}
		r, err := s.judge(ctx, tx, inv, profile, p, invalid)
		if err != nil {
			return err
		}
		if r != nil {
			p.Status, p.RejectionReason, p.Message = store.ProposalRejected, r.reason.String(), r.message
		}

		var created bool
		stored, created, err = s.Store.InsertProposal(ctx, tx, p)
		if err != nil || !created {
			return err
		}
		countProposal(tx, r)
		if r == nil || r.reason != themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH {
			return nil
		}
		return s.Store.RecordSafetyEvent(ctx, tx, store.SafetyEvent{
			IncidentID: inv.IncidentID, ProposalID: stored.ID, Kind: store.SafetyTargetMismatch, Detail: r.message,
		})
	})
	if err != nil {
		return nil, s.internal(ctx, err)
	}

	return proposalResponse(stored), nil
}

// parseProposal reads what every proposal must carry. invalid says why its
// parameters are wrong, for storing the proposal as rejected.
func parseProposal(req *themisv1.SubmitProposalRequest) (capability string, params map[string]any, invalid string, err error) {
	if req.GetTarget() == nil {
		return "", nil, "", status.Error(codes.InvalidArgument, "target is required")
	}
	if req.GetClientProposalId() == "" {
		return "", nil, "", status.Error(codes.InvalidArgument, "client_proposal_id is required")
	}
	switch c := req.GetCapability().(type) {
	case *themisv1.SubmitProposalRequest_RollbackToRevision:
		if c.RollbackToRevision.GetRevision() <= 0 {
			invalid = "The revision must be a positive number."
		}
		return CapabilityRollback, map[string]any{"revision": c.RollbackToRevision.GetRevision()}, invalid, nil
	default:
		return "", nil, "", status.Error(codes.InvalidArgument, "a capability is required")
	}
}

func proposalResponse(stored store.Proposal) *themisv1.SubmitProposalResponse {
	resp := &themisv1.SubmitProposalResponse{ProposalId: stored.ID, Message: stored.Message, Status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED}
	if stored.Status == store.ProposalRejected {
		resp.Status = themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED
		resp.RejectionReason = themisv1.RejectionReason(themisv1.RejectionReason_value[stored.RejectionReason])
	}
	return resp
}

// countProposal counts a stored proposal, and a rejection that is a safety
// event, once tx commits.
func countProposal(tx pgx.Tx, r *rejection) {
	if r == nil {
		store.AfterCommit(tx, proposalsTotal.WithLabelValues("accepted").Inc)
		return
	}
	store.AfterCommit(tx, proposalsTotal.WithLabelValues("rejected").Inc)
	if kind, ok := safetyKinds[r.reason]; ok {
		store.AfterCommit(tx, safetyEventsTotal.WithLabelValues(kind).Inc)
	}
}

// judge returns why a proposal must be rejected, or nil. The target must be
// exactly the incident's; a mismatch is a safety event, never corrected.
func (s *Server) judge(ctx context.Context, tx pgx.Tx, inv store.Investigation, profile config.Profile, p store.Proposal, invalid string) (*rejection, error) {
	if !slices.Contains(profile.Capabilities, p.Capability) {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED,
			fmt.Sprintf("The %s profile doesn't allow this kind of fix (%s).", profile.Name, p.Capability),
		}, nil
	}
	if p.Target != inv.Target {
		t := p.Target
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH,
			fmt.Sprintf("It names %s %s/%s (uid %s), but the incident is about %s/%s (uid %s).",
				t.Kind, t.Namespace, t.Name, t.UID, inv.Target.Namespace, inv.Target.Name, inv.Target.UID),
		}, nil
	}
	if invalid != "" {
		return &rejection{themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS, invalid}, nil
	}
	if len(p.CitedEvidenceIDs) == 0 {
		return &rejection{themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, "It cites no evidence."}, nil
	}
	missing, err := s.Store.MissingEvidence(ctx, tx, inv.IncidentID, p.CitedEvidenceIDs)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return &rejection{
			themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE,
			"It cites evidence Themis never collected: " + strings.Join(missing, ", ") + ".",
		}, nil
	}
	for _, name := range p.CitedPredicates {
		if !slices.Contains(predicateNames, name) {
			return &rejection{themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_PREDICATE, "It cites a check that doesn't exist: " + name + "."}, nil
		}
	}
	return nil, nil
}

func provenance(p *themisv1.Provenance) map[string]string {
	return map[string]string{"source": p.GetSource(), "model": p.GetModel(), "promptVersion": p.GetPromptVersion(), "route": p.GetRoute()}
}
