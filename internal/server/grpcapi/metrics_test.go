package grpcapi_test

import (
	"testing"

	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/server/grpcapi"
	"github.com/hzerrad/themis/internal/testutil/metrictest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func investigations(t *testing.T, outcome string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_investigations_total", map[string]string{"outcome": outcome})
}

func TestMetrics_TokenReviews(t *testing.T) {
	e := newEnv(t)
	agent := e.external(t)
	accepted := metrictest.Delta(t, "themis_token_reviews_total", map[string]string{"result": "accepted"})
	refused := metrictest.Delta(t, "themis_token_reviews_total", map[string]string{"result": "refused"})
	hits := metrictest.Delta(t, "themis_token_review_cache_hits_total", nil)
	limited := metrictest.Delta(t, "themis_requests_limited_total", map[string]string{"kind": "reads"})
	for range grpcapi.ReadsPerMinute + 1 {
		_ = list(t, agent)
	}
	_ = list(t, e.clientWith(t, "forged"))
	if a, r, h, l := accepted(), refused(), hits(), limited(); a != 1 || r != 1 || h != grpcapi.ReadsPerMinute || l != 1 {
		t.Errorf("accepted %v, refused %v, cache hits %v, limited %v", a, r, h, l)
	}
}

func TestMetrics_InvestigationProposed(t *testing.T) {
	e := newEnv(t)
	proposed := investigations(t, "proposed")
	accepted := metrictest.Delta(t, "themis_proposals_total", map[string]string{"result": "accepted"})
	inv := e.lease(t)
	e.submit(t, rollback(inv, "p-1"))
	e.submit(t, rollback(inv, "p-1")) // A retry stores nothing new.
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_PROPOSED, Summary: "rollback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := proposed(); got != 1 {
		t.Errorf("counted %v proposed investigations, want 1", got)
	}
	if got := accepted(); got != 1 {
		t.Errorf("counted %v accepted proposals, want 1", got)
	}
}

func TestMetrics_InvestigationFailures(t *testing.T) {
	for reason, outcome := range map[themisv1.InvestigationFailureReason]string{
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_MODEL_UNAVAILABLE: "failed",
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_BUDGET_EXHAUSTED:  "budget_exhausted",
		themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_DEADLINE_EXCEEDED: "budget_exhausted",
	} {
		t.Run(reason.String(), func(t *testing.T) {
			e := newEnv(t)
			counted := investigations(t, outcome)
			inv := e.lease(t)
			if _, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{LeaseId: inv.GetLeaseId(), Reason: reason}); err != nil {
				t.Fatal(err)
			}
			if got := counted(); got != 1 {
				t.Errorf("counted %v %s, want 1", got, outcome)
			}
		})
	}
}

func TestMetrics_AbstentionCountedByKind(t *testing.T) {
	blind := metrictest.Delta(t, "themis_abstentions_total", map[string]string{"kind": "blind"})
	ambiguous := metrictest.Delta(t, "themis_abstentions_total", map[string]string{"kind": "ambiguous"})

	e := newEnv(t)
	e.complete(t, e.lease(t), &themisv1.EvidenceGap{Template: evidence.ContainerLogsTail, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_DATA_CEILING})
	other := newEnv(t)
	other.complete(t, other.lease(t))

	if got := blind(); got != 1 {
		t.Errorf("counted %v blind abstentions, want 1", got)
	}
	if got := ambiguous(); got != 1 {
		t.Errorf("counted %v ambiguous abstentions, want 1", got)
	}
}
