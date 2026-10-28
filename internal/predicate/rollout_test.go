package predicate_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/predicate"
)

var (
	rolloutAt = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	now       = rolloutAt.Add(20 * time.Minute)
)

func item(t *testing.T, id, template string, view any) evidence.Item {
	t.Helper()
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	return evidence.Item{ID: id, Template: template, Data: data}
}

func history() evidence.RolloutHistoryView {
	return evidence.RolloutHistoryView{
		Deployment: evidence.DeploymentView{Name: "checkout", Revision: "2"},
		Revisions: []evidence.RevisionView{
			{ReplicaSet: "checkout-7d9f8b6c4", Revision: "2", Current: true, CreatedAt: rolloutAt, Replicas: 1},
			{ReplicaSet: "checkout-5c6a1e2f3", Revision: "1", CreatedAt: rolloutAt.Add(-7 * 24 * time.Hour), Replicas: 3, ReadyReplicas: 3},
		},
	}
}

func healthyPod(name, revision string) evidence.PodView {
	return evidence.PodView{Name: name, Revision: revision, Phase: "Running", Ready: true, Containers: []evidence.ContainerView{
		{Name: "app", Ready: true, State: &evidence.StateView{Status: "running"}},
	}}
}

func oomPod() evidence.PodView {
	exit := int32(137)
	return evidence.PodView{Name: "checkout-7d9f8b6c4-x2kqp", Revision: "2", Phase: "Running", Containers: []evidence.ContainerView{{
		Name: "app", RestartCount: 7,
		State:           &evidence.StateView{Status: "waiting", Reason: "CrashLoopBackOff"},
		LastTermination: &evidence.StateView{Status: "terminated", Reason: "OOMKilled", ExitCode: &exit, FinishedAt: rolloutAt.Add(14 * time.Minute)},
	}}}
}

func pods(p ...evidence.PodView) evidence.PodStatesView {
	return evidence.PodStatesView{Pods: p, TotalPods: len(p)}
}

func events(e ...evidence.EventView) evidence.TargetEventsView {
	return evidence.TargetEventsView{Events: e, TotalEvents: len(e)}
}

var backOff = evidence.EventView{
	Object: "Pod/checkout-7d9f8b6c4-x2kqp", Type: "Warning", Reason: "BackOff",
	FirstSeen: rolloutAt.Add(90 * time.Second), LastSeen: rolloutAt.Add(14 * time.Minute),
}

// scenario1 is the bad rollout: revision 2 is OOMKilled, revision 1 serves.
func scenario1(t *testing.T) evidence.Bundle {
	return evidence.Bundle{Items: []evidence.Item{
		item(t, "rh", evidence.RolloutHistory, history()),
		item(t, "ps", evidence.PodStates, pods(oomPod(),
			healthyPod("checkout-5c6a1e2f3-a1b2c", "1"), healthyPod("checkout-5c6a1e2f3-d3e4f", "1"))),
		item(t, "ev", evidence.TargetEvents, events(
			evidence.EventView{Object: "Deployment/checkout", Type: "Normal", Reason: "ScalingReplicaSet", FirstSeen: rolloutAt, LastSeen: rolloutAt},
			backOff,
		)),
	}}
}

func without(b evidence.Bundle, template string) evidence.Bundle {
	var out evidence.Bundle
	for _, it := range b.Items {
		if it.Template != template {
			out.Items = append(out.Items, it)
		}
	}
	return out
}

func replace(t *testing.T, b evidence.Bundle, template string, view any) evidence.Bundle {
	t.Helper()
	out := without(b, template)
	out.Items = append(out.Items, item(t, template+"-2", template, view))
	return out
}

func assertTruth(t *testing.T, r predicate.Result, want predicate.Truth) {
	t.Helper()
	if r.Value != want {
		t.Errorf("%s = %s (%s), want %s", r.Name, r.Value, r.Reason, want)
	}
	if r.Reason == "" {
		t.Errorf("%s has no reason", r.Name)
	}
}

func TestOOMKilledNewRevision(t *testing.T) {
	b := scenario1(t)
	r := predicate.OOMKilledNewRevision(b)
	assertTruth(t, r, predicate.True)
	if len(r.EvidenceIDs) == 0 || r.EvidenceIDs[0] != "ps" {
		t.Errorf("EvidenceIDs = %v, want ps", r.EvidenceIDs)
	}

	crashed := oomPod()
	crashed.Containers[0].LastTermination.Reason = "Error"
	assertTruth(t, predicate.OOMKilledNewRevision(replace(t, b, evidence.PodStates, pods(crashed))), predicate.False)

	// Only the old revision was OOMKilled: not the new revision's doing.
	old := oomPod()
	old.Revision = "1"
	assertTruth(t, predicate.OOMKilledNewRevision(replace(t, b, evidence.PodStates, pods(old, healthyPod("checkout-7d9f8b6c4-ab12c", "2")))), predicate.False)

	assertTruth(t, predicate.OOMKilledNewRevision(without(b, evidence.PodStates)), predicate.Unknown)
	assertTruth(t, predicate.OOMKilledNewRevision(replace(t, b, evidence.PodStates, pods())), predicate.Unknown)
}

func TestFailureOnsetAfterRollout(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, predicate.FailureOnsetAfterRollout(b), predicate.True)

	// The crash loop predates the rollout (scenario 3).
	early := evidence.EventView{
		Object: "Pod/checkout-5c6a1e2f3-a1b2c", Type: "Warning", Reason: "BackOff",
		FirstSeen: rolloutAt.Add(-3 * time.Hour), LastSeen: rolloutAt,
	}
	assertTruth(t, predicate.FailureOnsetAfterRollout(replace(t, b, evidence.TargetEvents, events(backOff, early))), predicate.False)

	assertTruth(t, predicate.FailureOnsetAfterRollout(without(b, evidence.RolloutHistory)), predicate.Unknown)
}

// A failure without a time could predate the rollout (Review Focus 5).
func TestTimelinePredicates_UnknownOnMissingTimestamps(t *testing.T) {
	undated := evidence.EventView{Object: "Pod/checkout-7d9f8b6c4-x2kqp", Type: "Warning", Reason: "BackOff"}
	crashed := oomPod()
	crashed.Containers[0].LastTermination.FinishedAt = time.Time{}
	b := replace(t, replace(t, scenario1(t), evidence.TargetEvents, events(undated)), evidence.PodStates, pods(crashed))

	assertTruth(t, predicate.FailureOnsetAfterRollout(b), predicate.Unknown)

	h := history()
	h.Revisions[0].CreatedAt = time.Time{}
	assertTruth(t, predicate.RolloutRecent(replace(t, scenario1(t), evidence.RolloutHistory, h), time.Hour, now), predicate.Unknown)
}

func TestFailureOnsetAfterRollout_NoFailureSeen(t *testing.T) {
	b := replace(t, replace(t, scenario1(t), evidence.TargetEvents, events()), evidence.PodStates, pods(healthyPod("checkout-7d9f8b6c4-x2kqp", "2")))
	assertTruth(t, predicate.FailureOnsetAfterRollout(b), predicate.Unknown)
}

func TestRolloutRecent(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, predicate.RolloutRecent(b, time.Hour, now), predicate.True)
	assertTruth(t, predicate.RolloutRecent(b, time.Hour, now.Add(24*time.Hour)), predicate.False)
	assertTruth(t, predicate.RolloutRecent(without(b, evidence.RolloutHistory), time.Hour, now), predicate.Unknown)
}

func TestPriorRevisionWasHealthy(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, predicate.PriorRevisionWasHealthy(b), predicate.True)

	// Both revisions fail (scenario 2).
	old := oomPod()
	old.Name, old.Revision = "checkout-5c6a1e2f3-a1b2c", "1"
	assertTruth(t, predicate.PriorRevisionWasHealthy(replace(t, b, evidence.PodStates, pods(oomPod(), old))), predicate.False)

	h := history()
	h.Revisions = h.Revisions[:1]
	assertTruth(t, predicate.PriorRevisionWasHealthy(replace(t, b, evidence.RolloutHistory, h)), predicate.Unknown)
}

// The prior revision was scaled down, so only its ReplicaSet is left to judge.
func TestPriorRevisionWasHealthy_ScaledDown(t *testing.T) {
	h := history()
	h.Revisions[1].Replicas, h.Revisions[1].ReadyReplicas = 0, 0
	b := replace(t, replace(t, scenario1(t), evidence.RolloutHistory, h), evidence.PodStates, pods(oomPod()))
	assertTruth(t, predicate.PriorRevisionWasHealthy(b), predicate.Unknown)
}

func TestGitOpsManaged(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, predicate.GitOpsManaged(b), predicate.False)

	argo := history()
	argo.Deployment.Markers = map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/checkout"}
	assertTruth(t, predicate.GitOpsManaged(replace(t, b, evidence.RolloutHistory, argo)), predicate.True)

	flux := history()
	flux.Deployment.Managers = []string{"kube-controller-manager", "kustomize-controller"}
	assertTruth(t, predicate.GitOpsManaged(replace(t, b, evidence.RolloutHistory, flux)), predicate.True)

	assertTruth(t, predicate.GitOpsManaged(without(b, evidence.RolloutHistory)), predicate.Unknown)
}

func TestEvaluate_EveryResultCitesEvidence(t *testing.T) {
	results := predicate.Evaluate(scenario1(t), now)
	names := map[string]bool{}
	for _, r := range results {
		if r.Name == "" || r.Version == "" {
			t.Errorf("result without name or version: %+v", r)
		}
		if names[r.Name] {
			t.Errorf("%s evaluated twice", r.Name)
		}
		names[r.Name] = true
		if r.Value != predicate.Unknown && len(r.EvidenceIDs) == 0 {
			t.Errorf("%s = %s cites no evidence", r.Name, r.Value)
		}
	}
	if len(results) != 5 {
		t.Errorf("Evaluate returned %d results, want 5", len(results))
	}
}

func TestTruth_String(t *testing.T) {
	for truth, want := range map[predicate.Truth]string{predicate.True: "true", predicate.False: "false", predicate.Unknown: "unknown"} {
		if truth.String() != want {
			t.Errorf("String() = %q, want %q", truth.String(), want)
		}
	}
	var zero predicate.Truth
	if zero != predicate.Unknown {
		t.Error("the zero Truth is not Unknown")
	}
}
