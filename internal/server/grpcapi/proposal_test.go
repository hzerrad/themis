package grpcapi_test

import (
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/incident/governance"
	"github.com/hzerrad/themis/internal/testutil/metrictest"
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

// Governance starts once the investigation is complete, never while it can
// still submit.
func TestComplete_Proposed_EnqueuesGovernInSameTx(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if pgtest.Count(t, e.pool, `SELECT count(*) FROM river_job WHERE kind = $1`, governance.GovernArgs{}.Kind()) != 0 {
		t.Fatal("a govern job before submitting")
	}
	accepted := e.submit(t, rollback(inv, "p-1"))
	if accepted.GetStatus() != themisv1.ProposalStatus_PROPOSAL_STATUS_ACCEPTED || accepted.GetProposalId() == "" {
		t.Fatalf("response = %v, want accepted", accepted)
	}
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

// A proposal Themis can't act on is rejected, counted and kept with its reason.
// One that points at something it shouldn't is counted as a safety event too.
func TestSubmit_Rejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		noCaps  bool // a profile without capabilities is diagnosis only
		change  func(*themisv1.SubmitProposalRequest)
		reason  themisv1.RejectionReason
		stored  string // the safety event kept with the proposal
		counted string // the safety event counted; "" counts none
	}{{
		name: "capability outside the candidates", noCaps: true,
		reason: themisv1.RejectionReason_REJECTION_REASON_CAPABILITY_NOT_ALLOWED, counted: "unknown_capability",
	}, {
		name: "unknown evidence",
		change: func(r *themisv1.SubmitProposalRequest) {
			r.CitedEvidenceIds = []string{"0192f0c1-0000-7000-8000-0000000000ee"}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, counted: "unknown_evidence",
	}, {
		name:   "no evidence cited",
		change: func(r *themisv1.SubmitProposalRequest) { r.CitedEvidenceIds = nil },
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_EVIDENCE, counted: "unknown_evidence",
	}, {
		name:   "unknown predicate",
		change: func(r *themisv1.SubmitProposalRequest) { r.CitedPredicates = []string{"cluster_is_fine"} },
		reason: themisv1.RejectionReason_REJECTION_REASON_UNKNOWN_PREDICATE,
	}, {
		name: "another target",
		change: func(r *themisv1.SubmitProposalRequest) {
			r.Target = &themisv1.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Uid: "u-coredns"}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH, stored: "target_mismatch", counted: "wrong_target",
	}, {
		// Same name, another UID: the object was recreated.
		name:   "recreated target",
		change: func(r *themisv1.SubmitProposalRequest) { r.Target.Uid = "uid-of-the-recreated-deployment" },
		reason: themisv1.RejectionReason_REJECTION_REASON_TARGET_MISMATCH, counted: "wrong_target",
	}, {
		name: "revision not positive",
		change: func(r *themisv1.SubmitProposalRequest) {
			r.Capability = &themisv1.SubmitProposalRequest_RollbackToRevision{RollbackToRevision: &themisv1.RollbackToRevisionParams{Revision: 0}}
		},
		reason: themisv1.RejectionReason_REJECTION_REASON_INVALID_PARAMS,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p := profile()
			if tc.noCaps {
				p.Capabilities = nil
			}
			e := newEnvWith(t, p)
			rejected := metrictest.Delta(t, "themis_proposals_total", map[string]string{"result": "rejected"})
			counted := map[string]func() float64{}
			for _, kind := range []string{"unknown_capability", "unknown_evidence", "wrong_target"} {
				counted[kind] = metrictest.Delta(t, "themis_safety_events_total", map[string]string{"kind": kind})
			}
			req := rollback(e.lease(t), "p-1")
			if tc.change != nil {
				tc.change(req)
			}
			resp := e.submit(t, req)
			assertRejected(t, resp, tc.reason)
			if tc.stored != "" {
				if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM safety_events WHERE proposal_id = $1 AND kind = $2`, resp.GetProposalId(), tc.stored); n != 1 {
					t.Errorf("%d %s safety events, want 1", n, tc.stored)
				}
			}
			if got := rejected(); got != 1 {
				t.Errorf("counted %v rejected proposals, want 1", got)
			}
			for kind, delta := range counted {
				want := 0.0
				if kind == tc.counted {
					want = 1
				}
				if got := delta(); got != want {
					t.Errorf("counted %v %s events, want %v", got, kind, want)
				}
			}
		})
	}
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

func TestSubmit_MissingTarget_InvalidArgument(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := rollback(inv, "p-1")
	req.Target = nil
	if _, err := e.proposals.SubmitProposal(t.Context(), req); code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

// A retry returns the stored proposal and never changes it, even when it says
// something else; a new client proposal ID is a new proposal.
func TestSubmit_IdempotentRetry(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	first := e.submit(t, rollback(inv, "p-1"))
	changed := rollback(inv, "p-1")
	changed.Rationale = "a changed mind"
	again := e.submit(t, changed)
	if again.GetProposalId() != first.GetProposalId() {
		t.Errorf("a retry created proposal %s, want %s", again.GetProposalId(), first.GetProposalId())
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals`); n != 1 {
		t.Errorf("%d proposals, want 1", n)
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals WHERE rationale = 'revision 2 lowered the memory limit to 64Mi'`); n != 1 {
		t.Error("a retry changed the stored proposal")
	}
	if other := e.submit(t, rollback(inv, "p-2")); other.GetProposalId() == first.GetProposalId() {
		t.Error("a new client proposal ID did not create a proposal")
	}
	if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM proposals`); n != 2 {
		t.Errorf("%d proposals, want 2", n)
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
