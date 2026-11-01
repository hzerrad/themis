package grpcserver_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/store"

	themisv1 "github.com/hzerrad/themis/gen/go/themis/v1"
)

// gap returns the only gap of the incident, failing unless there is exactly one.
func (e *env) gap(t *testing.T, incidentID string) store.EvidenceGap {
	t.Helper()
	gaps, err := e.store.EvidenceGaps(t.Context(), incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1: %+v", len(gaps), gaps)
	}
	return gaps[0]
}

// The investigator asked for evidence the profile does not allow: the profile,
// not the cluster, kept it from Themis.
func TestEvidence_NotInProfileRecordsGap(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.ContainerLogsTail, Params: map[string]string{"pod": "checkout-7d9f8b6c4-x2kqp"}}
	if _, err := e.evidence.CollectEvidence(t.Context(), req); err == nil {
		t.Fatal("a template outside the profile was collected")
	}
	g := e.gap(t, inv.GetIncidentId())
	if g.Cause != evidence.GapNotInProfile || g.Template != evidence.ContainerLogsTail || g.LeaseID != inv.GetLeaseId() || g.Params["pod"] != "checkout-7d9f8b6c4-x2kqp" {
		t.Errorf("gap = %+v", g)
	}
}

func TestEvidence_BudgetSpentRecordsGap(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	req := &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.PodStates}
	for range 2 {
		if _, err := e.evidence.CollectEvidence(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.TargetEvents}); err == nil {
		t.Fatal("a request past the budget was served")
	}
	g := e.gap(t, inv.GetIncidentId())
	if g.Cause != evidence.GapBudgetSpent || g.Template != evidence.TargetEvents {
		t.Errorf("gap = %+v, want target_events refused for the budget", g)
	}
}

// A collection that fails is answered as before, and its cause is kept.
func TestEvidence_CollectionFailureRecordsGap(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: e.target.Namespace, Name: e.target.Name}}
	if err := k8s.Delete(t.Context(), d); err != nil {
		t.Fatal(err)
	}

	resp, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.RolloutHistory})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetFailures()) != 1 || resp.GetFailures()[0].GetReason() != themisv1.CollectionFailureReason_COLLECTION_FAILURE_REASON_NOT_FOUND {
		t.Fatalf("failures = %v, want NOT_FOUND as before", resp.GetFailures())
	}
	g := e.gap(t, inv.GetIncidentId())
	if g.Cause != evidence.GapNotFound || g.Template != evidence.RolloutHistory || g.Detail == "" {
		t.Errorf("gap = %+v", g)
	}
}

// A request on a lost lease records nothing: there is no investigation to
// attribute it to.
func TestEvidence_StaleLeaseRecordsNoGap(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	_, _ = e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: "0192f0c1-0000-7000-8000-0000000000ff", Template: evidence.ContainerLogsTail})
	if gaps, _ := e.store.EvidenceGaps(t.Context(), inv.GetIncidentId()); len(gaps) != 0 {
		t.Errorf("gaps = %+v, want none", gaps)
	}
}

// Repeated refusals are one gap.
func TestEvidence_RepeatedRefusalRecordedOnce(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	for range 2 {
		if _, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.PodStates}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		_, _ = e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: evidence.TargetEvents})
	}
	if g := e.gap(t, inv.GetIncidentId()); g.Cause != evidence.GapBudgetSpent {
		t.Errorf("gap = %+v", g)
	}
}

// A template Themis does not have is refused, but it is not evidence that was
// kept from the investigation.
func TestEvidence_UnknownTemplateRecordsNoGap(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	if _, err := e.evidence.CollectEvidence(t.Context(), &themisv1.CollectEvidenceRequest{LeaseId: inv.GetLeaseId(), Template: "made_up"}); err == nil {
		t.Fatal("an unknown template was collected")
	}
	if gaps, _ := e.store.EvidenceGaps(t.Context(), inv.GetIncidentId()); len(gaps) != 0 {
		t.Errorf("gaps = %+v, want none", gaps)
	}
}

func (e *env) complete(t *testing.T, inv *themisv1.Investigation, gaps ...*themisv1.EvidenceGap) {
	t.Helper()
	_, err := e.investigations.CompleteInvestigation(t.Context(), &themisv1.CompleteInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Conclusion: themisv1.InvestigationConclusion_INVESTIGATION_CONCLUSION_INCONCLUSIVE,
		Summary: "the evidence doesn't point to one cause", Gaps: gaps,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An investigation that couldn't decide because the model may not see the
// logs was blind: the operator can change that.
func TestComplete_Inconclusive_StoresGapsAndKind(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.complete(t, inv, &themisv1.EvidenceGap{
		Template: evidence.ContainerLogsTail, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_DATA_CEILING,
		Detail: "logs are sensitive; this model may see internal data",
	})
	g := e.gap(t, inv.GetIncidentId())
	if g.Cause != evidence.GapDataCeiling || g.Template != evidence.ContainerLogsTail || g.LeaseID != inv.GetLeaseId() {
		t.Errorf("gap = %+v", g)
	}
	rec, err := e.store.IncidentInvestigation(t.Context(), inv.GetIncidentId())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Blind {
		t.Error("an investigation missing evidence Themis could have had is not blind")
	}
}

// Seeing everything and finding no clear answer is not blind.
func TestComplete_InconclusiveWithoutGapsIsAmbiguous(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.complete(t, inv)
	rec, err := e.store.IncidentInvestigation(t.Context(), inv.GetIncidentId())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Blind {
		t.Error("an investigation with nothing missing is blind")
	}
}

func TestFail_StoresReportedGaps(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	_, err := e.investigations.FailInvestigation(t.Context(), &themisv1.FailInvestigationRequest{
		LeaseId: inv.GetLeaseId(), Reason: themisv1.InvestigationFailureReason_INVESTIGATION_FAILURE_REASON_INVALID_MODEL_OUTPUT,
		Gaps: []*themisv1.EvidenceGap{{Template: evidence.ContainerLogsTail, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if g := e.gap(t, inv.GetIncidentId()); g.Cause != evidence.GapNotInProfile {
		t.Errorf("gap = %+v", g)
	}
}

// The investigator chooses the template and the cause: only templates Themis
// has, with causes it may report, are kept.
func TestComplete_ReportedGapsAreBounded(t *testing.T) {
	e := newEnv(t)
	inv := e.lease(t)
	e.complete(t, inv,
		&themisv1.EvidenceGap{Template: "made_up", Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_NOT_IN_PROFILE},
		&themisv1.EvidenceGap{Template: evidence.PodStates, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_UNSPECIFIED},
		&themisv1.EvidenceGap{Template: evidence.PodStates, Cause: themisv1.EvidenceGapCause_EVIDENCE_GAP_CAUSE_BUDGET_SPENT, Detail: strings.Repeat("x", 5000)},
	)
	g := e.gap(t, inv.GetIncidentId())
	if g.Template != evidence.PodStates || g.Cause != evidence.GapBudgetSpent || len(g.Detail) > 1024 {
		t.Errorf("gap = %+v (detail %d bytes)", g.Cause, len(g.Detail))
	}
}
