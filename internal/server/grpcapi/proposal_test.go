package grpcapi_test

import (
	"testing"

	"google.golang.org/grpc/codes"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func targetProto(inv *themisv1.Investigation) *themisv1.TargetRef {
	t := inv.GetTarget()
	return &themisv1.TargetRef{Group: t.GetGroup(), Kind: t.GetKind(), Namespace: t.GetNamespace(), Name: t.GetName(), Uid: t.GetUid()}
}

func rollback(inv *themisv1.Investigation, clientID string) *themisv1.SubmitProposalRequest {
	return &themisv1.SubmitProposalRequest{
		LeaseId:          inv.GetLeaseId(),
		ClientProposalId: clientID,
		Target:           targetProto(inv),
		ExpectedEffect:   "pods stop being OOMKilled",
		Rationale:        "revision 2 lowered the memory limit to 64Mi",
		CitedEvidenceIds: []string{inv.GetInitialEvidence()[0].GetId()},
		CitedPredicates:  []string{"oom_killed_new_revision"},
		Provenance:       &themisv1.Provenance{Source: "themis-intelligence", Model: "qwen3"},
		Capability:       &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 1}},
	}
}

// SubmitProposal says who is suggesting, from the lease or the caller's
// token, and answers in gRPC: a rejected suggestion is an answer, a refused
// request an error.
func TestSubmitProposal(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	inv := e.lease(t)
	with := func(clientID string, outside bool, change func(*themisv1.SubmitProposalRequest)) *themisv1.SubmitProposalRequest {
		r := rollback(inv, clientID)
		if outside {
			r = fromOutside(inv, clientID)
		}
		if change != nil {
			change(r)
		}
		return r
	}
	for _, tc := range []struct {
		name   string
		client themisv1.ProposalServiceClient
		req    *themisv1.SubmitProposalRequest
		code   codes.Code
		status themisv1.ProposalStatus
		source string // who the stored suggestion is from
	}{
		{
			name: "the investigator, under its lease", client: e.proposals, req: with("a", false, nil),
			status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED, source: "themis-intelligence",
		},
		{
			name: "an agent, by its token", client: agent, req: with("b", true, nil),
			status: themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED, source: "serviceaccount:" + agentAccount,
		},
		{
			name: "a rejected suggestion", client: e.proposals, req: with("c", false, func(r *themisv1.SubmitProposalRequest) { r.Target.Name = "coredns" }),
			status: themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED, source: "themis-intelligence",
		},
		{name: "the investigator naming an incident", client: e.proposals, req: with("d", true, nil), code: codes.Unauthenticated},
		{name: "no target", client: e.proposals, req: with("e", false, func(r *themisv1.SubmitProposalRequest) { r.Target = nil }), code: codes.InvalidArgument},
		{name: "an agent naming a lease", client: agent, req: with("f", false, nil), code: codes.PermissionDenied},
		{name: "an incident the agent can't see", client: agent, req: with("g", true, func(r *themisv1.SubmitProposalRequest) {
			r.IncidentId = "00000000-0000-7000-8000-000000000000"
		}), code: codes.NotFound},
		{name: "a lost lease", client: e.proposals, req: with("h", false, func(r *themisv1.SubmitProposalRequest) {
			r.LeaseId = "0192f0c1-0000-7000-8000-0000000000ff"
		}), code: codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.client.SubmitProposal(t.Context(), tc.req)
			if tc.code != codes.OK {
				wantCode(t, err, tc.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rejected := tc.status == themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED
			if resp.GetStatus() != tc.status || rejected != (resp.GetRejectionReason() == themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH) {
				t.Errorf("response = %v, want %s", resp, tc.status)
			}
			p, err := e.store.Proposal(t.Context(), e.store.DB(), resp.GetProposalId())
			if err != nil {
				t.Fatal(err)
			}
			if p.Provenance["source"] != tc.source || p.Message != resp.GetMessage() {
				t.Errorf("stored from %q saying %q, want from %q saying %q", p.Provenance["source"], p.Message, tc.source, resp.GetMessage())
			}
		})
	}
}
