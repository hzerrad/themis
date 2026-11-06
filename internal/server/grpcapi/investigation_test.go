package grpcapi_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident/investigation"
	"github.com/hzerrad/themis/internal/testutil/pgtest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// otherLease is a lease nobody holds.
const otherLease = "0192f0c1-0000-7000-8000-0000000000ff"

// wantStatus checks an RPC's code, and its message when want has one.
func wantStatus(t *testing.T, err error, want codes.Code, msg string) {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != want || (msg != "" && st.Message() != msg) {
		t.Errorf("status = %s %q, want %s %q", st.Code(), st.Message(), want, msg)
	}
}

// The investigator learns what it may read, check and suggest, described from
// the catalog; with nothing to lease, the answer is empty.
func TestLeaseInvestigation(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if inv.GetLeaseId() == "" || inv.GetIncidentId() == "" || inv.GetProfile() != "rollout-regression" || inv.GetTarget().GetUid() != e.target.UID {
		t.Errorf("investigation = %v", inv)
	}
	if !inv.GetLeaseDeadline().AsTime().Equal(e.now.Add(time.Minute)) || !inv.GetBudget().GetDeadline().AsTime().Equal(e.now.Add(config.DefaultInvestigationTimeout)) || inv.GetBudget().GetMaxSteps() != config.DefaultMaxSteps {
		t.Errorf("lease deadline %v, budget %v", inv.GetLeaseDeadline().AsTime(), inv.GetBudget())
	}
	if len(inv.GetInitialEvidence()) != 2 || len(inv.GetPredicates()) != 6 || inv.GetPredicates()[0].GetDescription() == "" {
		t.Errorf("%d items and predicates %v, want 2 items and 6 described predicates", len(inv.GetInitialEvidence()), inv.GetPredicates())
	}
	if len(inv.GetAllowedTemplates()) != 4 || inv.GetAllowedTemplates()[0].GetDescription() == "" || len(inv.GetAllowedTemplates()[3].GetParams()) == 0 {
		t.Errorf("allowed templates = %v, want 4 described, with their parameters", inv.GetAllowedTemplates())
	}
	if len(inv.GetCandidateCapabilities()) != 1 || len(inv.GetCapabilities()) != 1 || inv.GetGuidance() != profile().Guidance {
		t.Errorf("capabilities = %v, %v, guidance %q", inv.GetCandidateCapabilities(), inv.GetCapabilities(), inv.GetGuidance())
	}
	// The root causes of the profile's pack, described.
	if causes := inv.GetRootCauses(); len(causes) != 12 || causes[0].GetName() != "memory_limit_regression" || causes[0].GetDescription() == "" {
		t.Errorf("root causes = %v, want the rollout-regression pack's 12", causes)
	}

	again, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w-2"})
	if err != nil || again.GetInvestigation() != nil {
		t.Errorf("a second lease = %v, %v; want nothing", again, err)
	}
}

func TestRenewLease(t *testing.T) {
	for name, tc := range map[string]struct {
		after time.Duration
		code  codes.Code
	}{
		"a live lease":     {after: 50 * time.Second},
		"an expired lease": {after: 2 * time.Minute, code: codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			inv := e.lease(t)
			e.now = e.now.Add(tc.after)
			resp, err := e.investigations.RenewLease(t.Context(), &themisv1.RenewLeaseRequest{LeaseId: inv.GetLeaseId()})
			if tc.code != codes.OK {
				wantStatus(t, err, tc.code, "the lease is unknown, expired or superseded")
				return
			}
			if err != nil || !resp.GetLeaseDeadline().AsTime().Equal(e.now.Add(time.Minute)) {
				t.Errorf("RenewLease = %v, %v; want a minute from now", resp, err)
			}
		})
	}
}

// CollectEvidence answers with the item or why it couldn't be read, and says
// in gRPC why a request was refused.
func TestCollectEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		template string
		params   map[string]string
		lease    string
		before   func(*testing.T, *env, *themisv1.Investigation)
		code     codes.Code
		msg      string
		failure  themisv1.CollectionFailureReason
	}{
		"evidence": {template: evidence.TargetEvents},
		"a failed read": {
			template: evidence.RolloutHistory, failure: themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_FOUND,
			before: func(t *testing.T, e *env, _ *themisv1.Investigation) {
				if err := k8s.Delete(t.Context(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: e.target.Namespace, Name: e.target.Name}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"not in the profile": {
			template: evidence.ReferencedConfig, code: codes.PermissionDenied, msg: `template "referenced_config" is not allowed for the rollout-regression profile`,
		},
		"past the budget": {
			template: evidence.TargetEvents, code: codes.ResourceExhausted, msg: "the evidence budget is spent",
			before: func(t *testing.T, e *env, inv *themisv1.Investigation) {
				for range 2 {
					if _, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.PodStates}); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		"bad parameters": {template: evidence.ContainerLogsTail, params: map[string]string{"bogus": "x"}, code: codes.InvalidArgument},
		"a lost lease":   {template: evidence.TargetEvents, lease: otherLease, code: codes.FailedPrecondition, msg: "the lease is unknown, expired or superseded"},
		"a profile no longer configured": {
			template: evidence.TargetEvents, code: codes.FailedPrecondition, msg: `no profile of this incident is configured any more (rollout-regression)`,
			before: func(_ *testing.T, e *env, _ *themisv1.Investigation) {
				e.server.Investigations.Config = config.Config{}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			inv := e.lease(t)
			if tc.before != nil {
				tc.before(t, e, inv)
			}
			lease := inv.GetLeaseId()
			if tc.lease != "" {
				lease = tc.lease
			}
			resp, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: lease, Template: tc.template, Params: tc.params})
			switch {
			case tc.code != codes.OK:
				wantStatus(t, err, tc.code, tc.msg)
			case tc.failure != 0:
				f := resp.GetFailures()
				if err != nil || len(f) != 1 || f[0].GetReason() != tc.failure || f[0].GetTarget().GetUid() != e.target.UID || f[0].GetMessage() == "" || resp.GetRemainingRequests() != 2 {
					t.Errorf("CollectEvidence = %v, %v; want %s, the budget untouched", resp, err, tc.failure)
				}
			default:
				items := resp.GetItems()
				if err != nil || len(items) != 1 || items[0].GetId() == "" || items[0].GetTemplate() != tc.template || resp.GetRemainingRequests() != 1 {
					t.Errorf("CollectEvidence = %v, %v; want the item and 1 more", resp, err)
				}
			}
		})
	}
}

// CompleteInvestigation takes the conclusions an investigator can reach, with
// its result and the gaps it can report.
func TestCompleteInvestigation(t *testing.T) {
	logs := &themisv1.EvidenceGap{Template: evidence.ContainerLogsTail, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_DATA_CEILING, Detail: "logs are sensitive"}
	unspecified := &themisv1.EvidenceGap{Template: evidence.PodStates}
	for name, tc := range map[string]struct {
		req  *themisv1.CompleteInvestigationRequest
		code codes.Code
		want string // the conclusion stored
		gaps int
	}{
		"no conclusion": {req: &themisv1.CompleteInvestigationRequest{}, code: codes.InvalidArgument},
		"proposed": {
			req: &themisv1.CompleteInvestigationRequest{
				Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED, Summary: "rollback",
				Hypotheses: []*themisv1.Hypothesis{{RootCauseClass: "memory_limit_regression", Status: themisv1.HypothesisStatus_HYPOTHESIS_STATUS_SUPPORTED}},
			},
			want: investigation.Proposed,
		},
		"inconclusive, with gaps": {
			req: &themisv1.CompleteInvestigationRequest{
				Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE, Summary: "can't tell",
				Gaps: []*themisv1.EvidenceGap{logs, unspecified},
			},
			want: investigation.Inconclusive, gaps: 1,
		},
		"a lost lease": {
			req:  &themisv1.CompleteInvestigationRequest{LeaseId: otherLease, Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED},
			code: codes.FailedPrecondition,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			inv := e.lease(t)
			if tc.req.LeaseId == "" {
				tc.req.LeaseId = inv.GetLeaseId()
			}
			_, err := e.investigations.CompleteInvestigation(t.Context(), tc.req)
			if tc.code != codes.OK {
				wantStatus(t, err, tc.code, "")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n := pgtest.Count(t, e.pool, `SELECT count(*) FROM investigations WHERE incident_id = $1 AND conclusion = $2 AND summary = $3 AND jsonb_array_length(hypotheses) = $4`,
				inv.GetIncidentId(), tc.want, tc.req.GetSummary(), len(tc.req.GetHypotheses())); n != 1 {
				t.Errorf("the result isn't stored as %s", tc.want)
			}
			gaps, err := e.store.EvidenceGaps(t.Context(), inv.GetIncidentId())
			if err != nil || len(gaps) != tc.gaps || (tc.gaps == 1 && gaps[0].Cause != evidence.GapDataCeiling) {
				t.Errorf("gaps = %+v, %v; want only the data ceiling's", gaps, err)
			}
		})
	}
}

// FailInvestigation names every reason an investigator gives as one Themis
// can put in words.
func TestFailInvestigation(t *testing.T) {
	reasons := map[themisv1.InvestigationFailureReason]investigation.Reason{
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_UNSPECIFIED:          investigation.Unspecified,
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE:    investigation.ModelUnavailable,
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT: investigation.InvalidModelOutput,
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:     investigation.BudgetExhausted,
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED:    investigation.DeadlineExceeded,
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INTERNAL:             investigation.Internal,
	}
	if len(reasons) != len(themisv1.InvestigationFailureReason_name) {
		t.Fatalf("the table names %d reasons, the contract %d", len(reasons), len(themisv1.InvestigationFailureReason_name))
	}
	for r, want := range reasons {
		t.Run(r.String(), func(t *testing.T) {
			e := newEnv(t)
			inv := e.lease(t)
			if _, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{LeaseId: inv.GetLeaseId(), Reason: r, Message: "m"}); err != nil {
				t.Fatal(err)
			}
			var failure, kind string
			if err := e.pool.QueryRow(t.Context(), `SELECT failure, failure_kind FROM investigations WHERE incident_id = $1`, inv.GetIncidentId()).Scan(&failure, &kind); err != nil {
				t.Fatal(err)
			}
			if why, _, _ := strings.Cut(failure, ": m"); kind != string(want) || why == "" || strings.ToUpper(why[:1]) != why[:1] || !strings.HasSuffix(failure, ": m") {
				t.Errorf("failure = %q (%s), want a sentence, stored as %s", failure, kind, want)
			}
		})
	}
	e := newEnv(t)
	e.lease(t)
	_, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{LeaseId: otherLease})
	wantStatus(t, err, codes.FailedPrecondition, "the lease is unknown, expired or superseded")
}
