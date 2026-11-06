package suggestion

import (
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// Request reads a suggestion as the gRPC API and the HTTP API both take it:
// the fix as capability_name and params, or in the older
// rollback_to_revision form. What it lacks is refused when it is submitted.
func Request(req *themisv1.SubmitProposalRequest) Suggestion {
	t := req.GetTarget()
	p := req.GetProvenance()
	s := Suggestion{
		Lease: req.GetLeaseId(), Incident: req.GetIncidentId(), ClientID: req.GetClientProposalId(),
		Target:         incident.TargetRef{Group: t.GetGroup(), Kind: t.GetKind(), Namespace: t.GetNamespace(), Name: t.GetName(), UID: t.GetUid()},
		ExpectedEffect: req.GetExpectedEffect(), Rationale: req.GetRationale(),
		Evidence: req.GetCitedEvidenceIds(), Checks: req.GetCitedPredicates(),
		Provenance: map[string]string{"source": p.GetSource(), "model": p.GetModel(), "promptVersion": p.GetPromptVersion(), "route": p.GetRoute()},
		Notes:      req.GetNotes(),
	}
	switch {
	case t == nil:
		s.malformed = "target is required"
	case s.ClientID == "":
		s.malformed = "client_proposal_id is required"
	case req.GetCapabilityName() != "" && req.GetCapability() != nil:
		s.malformed = "set capability_name or rollback_to_revision, not both"
	case req.GetCapabilityName() != "":
		s.Fix, s.Params = req.GetCapabilityName(), req.GetParams().AsMap()
	default:
		older, ok := req.GetCapability().(*themisv1.SubmitProposalRequest_RollbackToRevision)
		if !ok {
			s.malformed = "a capability is required"
			break
		}
		revision := older.RollbackToRevision.GetRevision()
		s.Fix, s.Params = rollback.Name, map[string]any{"revision": revision}
		if revision <= 0 {
			s.invalid = "the revision must be a positive number."
		}
	}
	return s
}

// Response is what the suggester is told of a suggestion kept.
func Response(p store.Proposal) *themisv1.SubmitProposalResponse {
	resp := &themisv1.SubmitProposalResponse{ProposalId: p.ID, Message: p.Message, Status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED}
	if p.Status == store.ProposalRejected {
		resp.Status = themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED
		resp.RejectionReason = themisv1.RejectionReason(themisv1.RejectionReason_value[p.RejectionReason])
	}
	return resp
}
