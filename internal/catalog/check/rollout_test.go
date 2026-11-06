package check_test

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
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

// rollout is the checks the rollout-regression pack's profile runs, as the
// catalog compiles them.
var rollout = func() check.List {
	c, err := catalog.Themis(nil)
	if err != nil {
		panic(err)
	}
	p, ok := c.Profile("rollout-regression")
	if !ok {
		panic("no rollout-regression pack")
	}
	return c.Checks.For(p.Checks)
}()

// run evaluates the check named name on b, as of now.
func run(name string, b evidence.Bundle) check.Result { return runAt(name, b, now) }

func runAt(name string, b evidence.Bundle, at time.Time) check.Result {
	c, ok := rollout.Get(name)
	if !ok {
		panic("no check " + name)
	}
	return check.List{c}.Evaluate(b, at)[0]
}

func assertTruth(t *testing.T, r check.Result, want check.Truth) {
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
	r := run("oom_killed_new_revision", b)
	assertTruth(t, r, check.True)
	if len(r.EvidenceIDs) == 0 || r.EvidenceIDs[0] != "ps" {
		t.Errorf("EvidenceIDs = %v, want ps", r.EvidenceIDs)
	}

	crashed := oomPod()
	crashed.Containers[0].LastTermination.Reason = "Error"
	assertTruth(t, run("oom_killed_new_revision", replace(t, b, evidence.PodStates, pods(crashed))), check.False)

	// Only the old revision was OOMKilled: not the new revision's doing.
	old := oomPod()
	old.Revision = "1"
	assertTruth(t, run("oom_killed_new_revision", replace(t, b, evidence.PodStates, pods(old, healthyPod("checkout-7d9f8b6c4-ab12c", "2")))), check.False)

	assertTruth(t, run("oom_killed_new_revision", without(b, evidence.PodStates)), check.Unknown)
	assertTruth(t, run("oom_killed_new_revision", replace(t, b, evidence.PodStates, pods())), check.Unknown)
}

func TestFailureOnsetAfterRollout(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, run("failure_onset_after_rollout", b), check.True)

	// The crash loop predates the rollout (scenario 3).
	early := evidence.EventView{
		Object: "Pod/checkout-5c6a1e2f3-a1b2c", Type: "Warning", Reason: "BackOff",
		FirstSeen: rolloutAt.Add(-3 * time.Hour), LastSeen: rolloutAt,
	}
	assertTruth(t, run("failure_onset_after_rollout", replace(t, b, evidence.TargetEvents, events(backOff, early))), check.False)

	assertTruth(t, run("failure_onset_after_rollout", without(b, evidence.RolloutHistory)), check.Unknown)

	// Anyone may write an event: one the pods don't bear out, ready and never
	// restarted, can't make it true.
	healthy := replace(t, b, evidence.PodStates, pods(healthyPod("checkout-7d9f8b6c4-x2kqp", "2")))
	assertTruth(t, run("failure_onset_after_rollout", healthy), check.Unknown)

	// Without the events an earlier failure could be hiding there: the pods
	// alone can't make it true, though they can still show it false.
	assertTruth(t, run("failure_onset_after_rollout", without(b, evidence.TargetEvents)), check.Unknown)
	crashedBefore := oomPod()
	crashedBefore.Containers[0].LastTermination.FinishedAt = rolloutAt.Add(-time.Hour)
	assertTruth(t, run("failure_onset_after_rollout", replace(t, without(b, evidence.TargetEvents), evidence.PodStates, pods(crashedBefore))), check.False)
}

// A Deployment's first revision replaced none: its creation is no rollout to
// blame, nor one recent, and there is nothing healthy to go back to.
func TestFirstRevision(t *testing.T) {
	h := history()
	h.Revisions = h.Revisions[:1]
	b := replace(t, scenario1(t), evidence.RolloutHistory, h)
	for _, name := range []string{"failure_onset_after_rollout", "rollout_recent", "earlier_revision_healthy"} {
		assertTruth(t, run(name, b), check.False)
	}
	if r := run("earlier_revision_healthy", b); r.Reason != "checkout has no earlier revision to roll back to" {
		t.Errorf("reason = %q, want it to say there is nothing to roll back to", r.Reason)
	}
}

// A failure without a time could predate the rollout (Review Focus 5).
func TestTimelinePredicates_UnknownOnMissingTimestamps(t *testing.T) {
	undated := evidence.EventView{Object: "Pod/checkout-7d9f8b6c4-x2kqp", Type: "Warning", Reason: "BackOff"}
	crashed := oomPod()
	crashed.Containers[0].LastTermination.FinishedAt = time.Time{}
	b := replace(t, replace(t, scenario1(t), evidence.TargetEvents, events(undated)), evidence.PodStates, pods(crashed))

	assertTruth(t, run("failure_onset_after_rollout", b), check.Unknown)

	h := history()
	h.Revisions[0].CreatedAt = time.Time{}
	assertTruth(t, runAt("rollout_recent", replace(t, scenario1(t), evidence.RolloutHistory, h), now), check.Unknown)
}

func TestFailureOnsetAfterRollout_NoFailureSeen(t *testing.T) {
	b := replace(t, replace(t, scenario1(t), evidence.TargetEvents, events()), evidence.PodStates, pods(healthyPod("checkout-7d9f8b6c4-x2kqp", "2")))
	assertTruth(t, run("failure_onset_after_rollout", b), check.Unknown)
}

func TestRolloutRecent(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, runAt("rollout_recent", b, now), check.True)
	assertTruth(t, runAt("rollout_recent", b, now.Add(24*time.Hour)), check.False)
	assertTruth(t, runAt("rollout_recent", without(b, evidence.RolloutHistory), now), check.Unknown)
}

func TestEarlierRevisionHealthy(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, run("earlier_revision_healthy", b), check.True)

	// Both revisions fail (scenario 2).
	old := oomPod()
	old.Name, old.Revision = "checkout-5c6a1e2f3-a1b2c", "1"
	assertTruth(t, run("earlier_revision_healthy", replace(t, b, evidence.PodStates, pods(oomPod(), old))), check.False)

	assertTruth(t, run("earlier_revision_healthy", without(b, evidence.RolloutHistory)), check.Unknown)
}

// The prior revision was scaled down, so only its ReplicaSet is left to judge.
func TestEarlierRevisionHealthy_ScaledDown(t *testing.T) {
	h := history()
	h.Revisions[1].Replicas, h.Revisions[1].ReadyReplicas = 0, 0
	b := replace(t, replace(t, scenario1(t), evidence.RolloutHistory, h), evidence.PodStates, pods(oomPod()))
	assertTruth(t, run("earlier_revision_healthy", b), check.Unknown)
}

func TestGitOpsManaged(t *testing.T) {
	b := scenario1(t)
	assertTruth(t, run("gitops_managed", b), check.False)

	argo := history()
	argo.Deployment.Markers = map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/checkout"}
	assertTruth(t, run("gitops_managed", replace(t, b, evidence.RolloutHistory, argo)), check.True)

	flux := history()
	flux.Deployment.Managers = []string{"kube-controller-manager", "kustomize-controller"}
	assertTruth(t, run("gitops_managed", replace(t, b, evidence.RolloutHistory, flux)), check.True)

	assertTruth(t, run("gitops_managed", without(b, evidence.RolloutHistory)), check.Unknown)
}

func TestRollout_EveryResultCitesEvidence(t *testing.T) {
	results := rollout.Evaluate(scenario1(t), now)
	names := map[string]bool{}
	for _, r := range results {
		if r.Name == "" || r.Version == "" {
			t.Errorf("result without name or version: %+v", r)
		}
		if names[r.Name] {
			t.Errorf("%s evaluated twice", r.Name)
		}
		names[r.Name] = true
		if r.Value != check.Unknown && len(r.EvidenceIDs) == 0 {
			t.Errorf("%s = %s cites no evidence", r.Name, r.Value)
		}
	}
	if len(results) != 6 {
		t.Errorf("Evaluate returned %d results, want 6", len(results))
	}
}

func TestTruth_String(t *testing.T) {
	for truth, want := range map[check.Truth]string{check.True: "true", check.False: "false", check.Unknown: "unknown"} {
		if truth.String() != want {
			t.Errorf("String() = %q, want %q", truth.String(), want)
		}
	}
	var zero check.Truth
	if zero != check.Unknown {
		t.Error("the zero Truth is not Unknown")
	}
}

func TestTruth_JSON(t *testing.T) {
	b, err := json.Marshal([]check.Truth{check.True, check.Unknown})
	if err != nil || string(b) != `["true","unknown"]` {
		t.Errorf("Marshal = %s, %v", b, err)
	}
}

// Rolling out a template seen before reuses its old ReplicaSet: the rollout
// starts when the template changed, not when the ReplicaSet was created.
func TestRolloutTime_ReusedReplicaSet(t *testing.T) {
	h := history()
	h.Revisions[0].CreatedAt = rolloutAt.Add(-48 * time.Hour)
	h.Deployment.TemplateChangedAt = rolloutAt
	b := replace(t, scenario1(t), evidence.RolloutHistory, h)

	assertTruth(t, runAt("rollout_recent", b, now), check.True)
	assertTruth(t, run("failure_onset_after_rollout", b), check.True)
}

func TestConfigChangedBeforeFailure(t *testing.T) {
	cm := func(at time.Time) evidence.ConfigRefView {
		return evidence.ConfigRefView{Kind: evidence.ConfigKindConfigMap, Name: "settings", ChangedAt: at}
	}
	failedAt := rolloutAt.Add(10 * time.Minute)
	crashed := func(restarts int32) evidence.PodStatesView {
		return evidence.PodStatesView{Pods: []evidence.PodView{{Name: "p", Revision: "2", StartedAt: rolloutAt, Containers: []evidence.ContainerView{
			{Name: "app", RestartCount: restarts, LastTermination: &evidence.StateView{Reason: "Error", FinishedAt: failedAt}},
		}}}}
	}
	// A warning event about the pod, three minutes after the rollout.
	early := []evidence.EventView{{Object: "Pod/p", Type: "Warning", Reason: "BackOff", FirstSeen: rolloutAt.Add(3 * time.Minute)}}
	tests := []struct {
		name   string
		refs   []evidence.ConfigRefView
		want   check.Truth
		reason string
		// restarts are the container's; events, what target_events says.
		restarts int32
		events   []evidence.EventView
	}{
		{"nothing referenced", nil, check.False, "", 0, nil},
		{"changed before the rollout", []evidence.ConfigRefView{cm(rolloutAt.Add(-time.Hour))}, check.False, "", 0, nil},
		{"written with the rollout", []evidence.ConfigRefView{cm(rolloutAt.Add(30 * time.Second))}, check.False, "", 0, nil},
		{"changed between the rollout and the failure", []evidence.ConfigRefView{cm(rolloutAt.Add(5 * time.Minute))}, check.True, "", 0, nil},
		{"changed after the failure", []evidence.ConfigRefView{cm(failedAt.Add(time.Minute))}, check.False, "", 0, nil},
		{"doesn't say when", []evidence.ConfigRefView{cm(time.Time{})}, check.Unknown, "", 0, nil},
		{"required and missing", []evidence.ConfigRefView{{Kind: evidence.ConfigKindConfigMap, Name: "gone", Missing: true}}, check.Unknown, "", 0, nil},
		{"optional and missing", []evidence.ConfigRefView{{Kind: evidence.ConfigKindConfigMap, Name: "gone", Missing: true, Optional: true}}, check.False, "", 0, nil},
		{"a Secret can't be checked", []evidence.ConfigRefView{{Kind: evidence.ConfigKindSecret, Name: "db"}}, check.False, "1 referenced Secret(s) can't be checked", 0, nil},
		// The status dates a container's only failure: an event anyone could
		// write can't move it earlier.
		{"an earlier event the status dates otherwise", []evidence.ConfigRefView{cm(rolloutAt.Add(5 * time.Minute))}, check.True, "", 1, early},
		// A container that restarted many times failed first before its last
		// end: an event about its pod, since it started, dates that.
		{"restarted many times, dated by an event", []evidence.ConfigRefView{cm(rolloutAt.Add(5 * time.Minute))}, check.False, "", 7, early},
		{"restarted many times, no event", []evidence.ConfigRefView{cm(rolloutAt.Add(5 * time.Minute))}, check.Unknown, "no failure with a known time", 7, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := evidence.Bundle{Items: []evidence.Item{
				item(t, "cfg", evidence.ReferencedConfig, evidence.ReferencedConfigView{Refs: tt.refs}),
				item(t, "h", evidence.RolloutHistory, history()),
				item(t, "p", evidence.PodStates, crashed(tt.restarts)),
				item(t, "e", evidence.TargetEvents, events(tt.events...)),
			}}
			got := run("config_changed_before_failure", b)
			if got.Value != tt.want {
				t.Errorf("value = %s (%s), want %s", got.Value, got.Reason, tt.want)
			}
			if !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("reason = %q, want it to say %q", got.Reason, tt.reason)
			}
		})
	}
	// Without the list of referenced config, nothing can be said.
	if got := run("config_changed_before_failure", evidence.Bundle{}); got.Value != check.Unknown {
		t.Errorf("without the list: value = %s, want unknown", got.Value)
	}
}
