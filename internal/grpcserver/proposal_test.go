package grpcserver_test

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/hzerrad/themis/internal/governance"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

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

func (e *env) submit(t *testing.T, req *themisv1.SubmitProposalRequest) *themisv1.SubmitProposalResponse {
	t.Helper()
	resp, err := e.proposals.SubmitProposal(t.Context(), req)
	if err != nil {
		t.Fatalf("SubmitProposal: %v", err)
	}
	return resp
}

func assertRejected(t *testing.T, resp *themisv1.SubmitProposalResponse, reason themisv1.RejectionReason) {
	t.Helper()
	if resp.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_REJECTED || resp.GetRejectionReason() != reason {
		t.Errorf("response = %v, want rejected with %s", resp, reason)
	}
	if resp.GetProposalId() == "" {
		t.Error("a rejected proposal was not kept")
	}
}

func TestSubmit_Valid_Accepted(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	resp := e.submit(t, rollback(inv, "p-1"))
	if resp.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED || resp.GetProposalId() == "" {
		t.Errorf("response = %v, want accepted", resp)
	}
}

// Governance starts once the investigation is complete, never while it can
// still submit.
func TestComplete_Proposed_EnqueuesGovernInSameTx(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1`, governance.GovernArgs{}.Kind()) != 0 {
		t.Fatal("a govern job before submitting")
	}
	accepted := e.submit(t, rollback(inv, "p-1"))
	if pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1`, governance.GovernArgs{}.Kind()) != 0 {
		t.Error("a govern job before the investigation completed")
	}

	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED,
		Summary: "rollback", Hypotheses: []*themisv1.Hypothesis{{RootCauseClass: "memory_limit_regression", Status: themisv1.HypothesisStatus_HYPOTHESIS_STATUS_SUPPORTED}},
	})
	if err != nil {
		t.Fatalf("CompleteInvestigation: %v", err)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.Diagnosed {
		t.Errorf("state = %s, want diagnosed", got)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'proposalID' = $2 AND args->>'incidentID' = $3`, governance.GovernArgs{}.Kind(), accepted.GetProposalId(), inv.GetIncidentId()); n != 1 {
		t.Errorf("%d govern jobs for the proposal and its incident, want 1", n)
	}
}

func TestComplete_ProposedWithoutAcceptedProposal_ClosesWithoutAction(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.CitedEvidenceIds = []string{"0192f0c1-0000-7000-8000-0000000000ee"}
	e.submit(t, req)

	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, inv.GetIncidentId()); got != incident.ClosedNoAction {
		t.Errorf("state = %s, want closed_no_action", got)
	}
}

// A profile without capabilities is diagnosis only.
func TestSubmit_CapabilityOutsideCandidates_StoredRejected(t *testing.T) {
	p := profile()
	p.Capabilities = nil
	e := newEnvWith(t, p)
	inv := e.lease(t)
	assertRejected(t, e.submit(t, rollback(inv, "p-1")), themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED)
}

func TestSubmit_NoCapability_InvalidArgument(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Capability = nil
	if _, err := e.proposals.SubmitProposal(t.Context(), req); code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestSubmit_UnknownEvidenceID_StoredRejected(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.CitedEvidenceIds = []string{"0192f0c1-0000-7000-8000-0000000000ee"}
	assertRejected(t, e.submit(t, req), themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE)
}

func TestSubmit_NoEvidenceCited_Rejected(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.CitedEvidenceIds = nil
	assertRejected(t, e.submit(t, req), themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE)
}

func TestSubmit_UnknownPredicate_Rejected(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.CitedPredicates = []string{"cluster_is_fine"}
	assertRejected(t, e.submit(t, req), themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_PREDICATE)
}

func TestSubmit_TargetMismatch_StoredRejectedWithSafetyEvent(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Target = &themisv1.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Uid: "u-coredns"}

	resp := e.submit(t, req)
	assertRejected(t, resp, themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH)
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM safety_events WHERE proposal_id = $1 AND kind = 'target_mismatch'`, resp.GetProposalId()); n != 1 {
		t.Errorf("%d safety events, want 1", n)
	}
}

// Same name, another UID: the object was recreated.
func TestSubmit_TargetUIDMismatch_Rejected(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Target.Uid = "uid-of-the-recreated-deployment"
	assertRejected(t, e.submit(t, req), themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH)
}

func TestSubmit_MissingTarget_InvalidArgument(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Target = nil
	if _, err := e.proposals.SubmitProposal(t.Context(), req); code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestSubmit_RevisionNotPositive_RejectedInvalidParams(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Capability = &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 0}}
	assertRejected(t, e.submit(t, req), themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS)
}

func TestSubmit_IdempotentRetry(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	first := e.submit(t, rollback(inv, "p-1"))
	again := e.submit(t, rollback(inv, "p-1"))
	if again.GetProposalId() != first.GetProposalId() {
		t.Errorf("a retry created proposal %s, want %s", again.GetProposalId(), first.GetProposalId())
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals`); n != 1 {
		t.Errorf("%d proposals, want 1", n)
	}
}

func TestSubmit_ProposalImmutable(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.submit(t, rollback(inv, "p-1"))
	changed := rollback(inv, "p-2")
	changed.Rationale = "a changed mind"
	e.submit(t, changed)
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals WHERE rationale = 'revision 2 lowered the memory limit to 64Mi'`); n != 1 {
		t.Error("the first proposal was changed")
	}
}

func TestSubmit_StaleLease_FailedPrecondition(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.LeaseId = "0192f0c1-0000-7000-8000-0000000000ff"
	if _, err := e.proposals.SubmitProposal(t.Context(), req); code(err) != codes.FailedPrecondition {
		t.Errorf("err = %v, want FailedPrecondition", err)
	}
}
