package grpcserver_test

import (
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/testutil/metrictest"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

func investigations(t *testing.T, outcome string) func() float64 {
	t.Helper()
	return metrictest.Delta(t, "themis_investigations_total", map[string]string{"outcome": outcome})
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

func TestMetrics_InvestigationAbstained(t *testing.T) {
	e := newEnv(t)
	abstained := investigations(t, "abstained")
	inv := e.lease(t)
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE, Summary: "unsure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := abstained(); got != 1 {
		t.Errorf("counted %v abstentions, want 1", got)
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

// A lease that expires is lost, whether it is leased again or was the last.
func TestMetrics_InvestigationLeaseLost(t *testing.T) {
	e := newEnv(t)
	lost := investigations(t, "lease_lost")
	for range 3 {
		e.lease(t)
		e.clock.now = e.clock.now.Add(time.Hour)
	}
	if _, err := e.investigations.LeaseInvestigation(t.Context(), &themisv1.LeaseInvestigationRequest{WorkerId: "w-1"}); err != nil {
		t.Fatal(err)
	}
	if got := lost(); got != 3 {
		t.Errorf("counted %v lost leases, want 3", got)
	}
}

func TestMetrics_SafetyEvents(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	wrongTarget := metrictest.Delta(t, "themis_safety_events_total", map[string]string{"kind": "wrong_target"})
	unknownEvidence := metrictest.Delta(t, "themis_safety_events_total", map[string]string{"kind": "unknown_evidence"})
	rejected := metrictest.Delta(t, "themis_proposals_total", map[string]string{"result": "rejected"})

	req := rollback(inv, "p-1")
	req.Target = &themisv1.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "kube-system", Name: "coredns", Uid: "u-coredns"}
	e.submit(t, req)
	req = rollback(inv, "p-2")
	req.CitedEvidenceIds = []string{"01a18d24-9ab1-7c3c-b65f-1ee86ac42758"}
	e.submit(t, req)
	req = rollback(inv, "p-3")
	req.CitedPredicates = []string{"made_up"}
	e.submit(t, req)

	if got := wrongTarget(); got != 1 {
		t.Errorf("counted %v wrong_target events, want 1", got)
	}
	if got := unknownEvidence(); got != 1 {
		t.Errorf("counted %v unknown_evidence events, want 1", got)
	}
	// An unknown predicate is rejected but is not a safety event.
	if got := rejected(); got != 3 {
		t.Errorf("counted %v rejected proposals, want 3", got)
	}
}

func TestMetrics_UnknownCapabilityIsASafetyEvent(t *testing.T) {
	p := profile()
	p.Capabilities = nil
	e := newEnvWith(t, p)
	unknown := metrictest.Delta(t, "themis_safety_events_total", map[string]string{"kind": "unknown_capability"})
	e.submit(t, rollback(e.lease(t), "p-1"))
	if got := unknown(); got != 1 {
		t.Errorf("counted %v unknown_capability events, want 1", got)
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
