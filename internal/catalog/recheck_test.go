package catalog_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/catalog/evidence/metrics"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
	"github.com/hzerrad/themis/scenarios"
)

// throttling is a metrics source that knows only how often search's app
// container was throttled, and records the windows it was asked over.
type throttling struct {
	ratio   float64
	windows []time.Duration
}

func (*throttling) Name() string { return metrics.Prometheus }

func (s *throttling) Throttling(_ context.Context, _ metrics.Pods, w time.Duration) (metrics.Reading, error) {
	s.windows = append(s.windows, w)
	return metrics.Reading{Containers: map[string]float64{"app": s.ratio}}, nil
}

func (*throttling) Memory(context.Context, metrics.Pods) (metrics.Reading, error) {
	return metrics.Reading{Unavailable: "none"}, nil
}

func (*throttling) PeakMemory(context.Context, metrics.Pods, time.Duration) (metrics.Reading, error) {
	return metrics.Reading{Unavailable: "none"}, nil
}

func (*throttling) History(context.Context, metrics.ReplicaSet, time.Time) (metrics.Record, error) {
	return metrics.Record{Unavailable: "none"}, nil
}

func (*throttling) Use(context.Context, metrics.ReplicaSet, time.Time, time.Time) (metrics.Use, error) {
	return metrics.Use{Unavailable: "none"}, nil
}

func (*throttling) CPU(context.Context, metrics.Pods) (metrics.Reading, error) {
	return metrics.Reading{Unavailable: "none"}, nil
}

var raisedAt = time.Date(2026, 11, 6, 21, 48, 0, 0, time.UTC)

// raised is search after Themis raised its CPU limit to 300m: revision 2,
// rolled out at raisedAt, runs one ready pod, and revision 1's pods are gone.
func raised(t *testing.T) (client.Reader, *workload.Workload) {
	t.Helper()
	labels := map[string]string{"app": "search"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "search", UID: "uid-search", Generation: 2,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: "busybox:1.37",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")}},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	replicaSet := func(revision, name string, at time.Time, limit string) *appsv1.ReplicaSet {
		tmpl := *d.Spec.Template.DeepCopy()
		tmpl.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse(limit)
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "shop", Name: name, UID: types.UID("uid-" + name), Labels: labels, OwnerReferences: owner,
				CreationTimestamp: metav1.NewTime(at), Annotations: map[string]string{"deployment.kubernetes.io/revision": revision},
			},
			Spec: appsv1.ReplicaSetSpec{Selector: d.Spec.Selector, Template: tmpl},
		}
	}
	old, current := replicaSet("1", "search-1", raisedAt.Add(-time.Hour), "200m"), replicaSet("2", "search-2", raisedAt, "300m")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "search-2-a", Labels: labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(current, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
	c := fake.NewClientBuilder().WithObjects(d, old, current, pod).Build()
	w, err := workload.Read(t.Context(), c, incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "search", UID: "uid-search"})
	if err != nil {
		t.Fatal(err)
	}
	return c, w
}

func withSource(t *testing.T, source metrics.Source) catalog.Catalog {
	t.Helper()
	cat, err := catalog.Build(nil, source, scenarios.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// A fix is verified from what the workload did since the change: throttling,
// a rate, is read over the time since the change in whole minutes, and never
// over less than the 5 minutes a rate needs; the pods' states need no
// window.
func TestRecheck_ReadsSinceTheChange(t *testing.T) {
	source := &throttling{ratio: 0.02}
	cat := withSource(t, source)
	if got := cat.SignalWindow([]string{"cpu_throttled_at_limit"}); got != 5*time.Minute {
		t.Errorf("throttling's signal window = %s, want 5m", got)
	}
	if got := cat.SignalWindow([]string{"oom_killed_current_revision"}); got != 0 {
		t.Errorf("OOM kills' signal window = %s, want none", got)
	}
	c, w := raised(t)
	now := raisedAt.Add(7*time.Minute + 23*time.Second)
	got := cat.Recheck(t.Context(), c, w, []string{"cpu_throttled_at_limit"}, now.Sub(raisedAt), now)
	if len(got) != 1 || got[0].Value != check.False || got[0].Reason != "app was throttled in 2% of its CPU periods over 7m, at its limit of 0.3 CPU, below 25%" {
		t.Errorf("since the change = %+v", got)
	}
	cat.Recheck(t.Context(), c, w, []string{"cpu_throttled_at_limit"}, 2*time.Minute, now)
	if want := []time.Duration{7 * time.Minute, 5 * time.Minute}; len(source.windows) != 2 || source.windows[0] != want[0] || source.windows[1] != want[1] {
		t.Errorf("read over %v, want %v", source.windows, want)
	}
	// Without a metrics source, it can't be told.
	got = withSource(t, nil).Recheck(t.Context(), c, w, []string{"cpu_throttled_at_limit"}, 7*time.Minute, now)
	if len(got) != 1 || got[0].Value != check.Unknown {
		t.Errorf("without a metrics source = %+v, want unknown", got)
	}
}

// When the workload runs Themis's own fix, the checks read it: the rollout
// was Themis's, and the revision it replaced failed, as the incident it
// fixed found, even once its pods are gone.
func TestDiagnose_ThemisFixIsEvidence(t *testing.T) {
	cat := withSource(t, &throttling{ratio: 0.57})
	c, w := raised(t)
	p, _ := cat.Profile("cpu-throttling")
	// Themis's own checks that read the record too, which the pack doesn't run.
	p.Checks = append(slices.Clone(p.Checks), "earlier_revision_healthy", "rollout_recent")
	at := catalog.Moment{Now: raisedAt.Add(4 * time.Minute)}
	results := func(d catalog.Diagnosis) map[string]check.Result {
		out := map[string]check.Result{}
		for _, r := range d.Results {
			out[r.Name] = r
		}
		return out
	}

	without := results(cat.Diagnose(t.Context(), c, w, config.Matched{p}, at))
	for _, name := range []string{"earlier_revision_healthy", "throttling_came_with_rollout"} {
		if r := without[name]; r.Value != check.Unknown {
			t.Errorf("without Themis's record or a history, %s = %+v, want unknown", name, r)
		}
	}

	fix, err := evidence.NewThemisFix(evidence.ThemisFixView{
		Incident: "inc-1", Fix: "deployment.set-resources", Done: "Raised search's CPU limit from 200m to 300m.", AppliedAt: raisedAt,
		Replaced: "1", Found: []evidence.FoundView{{Check: "cpu_throttled_at_limit", Reason: "app was throttled in 63% of its CPU periods over 1h, at its limit of 0.2 CPU"}},
	}, w.Target, at.Now)
	if err != nil {
		t.Fatal(err)
	}
	with := results(cat.Diagnose(t.Context(), c, w, config.Matched{p}, at, fix))
	healthy := with["earlier_revision_healthy"]
	if healthy.Value != check.False || healthy.Reason != "revision 1 failed before Themis's fix on incident inc-1 replaced it: app was throttled in 63% of its CPU periods over 1h, at its limit of 0.2 CPU" {
		t.Errorf("earlier_revision_healthy = %+v", healthy)
	}
	recent := with["rollout_recent"]
	if recent.Value != check.True || !strings.HasSuffix(recent.Reason, "ago, as Themis's fix on incident inc-1") {
		t.Errorf("rollout_recent = %+v", recent)
	}
	// The version the fix replaced was throttled: the rollout didn't bring
	// it, so a raise isn't refused for it.
	came := with["throttling_came_with_rollout"]
	if came.Value != check.False || !strings.Contains(came.Reason, "revision 1 was throttled before Themis's fix on incident inc-1 replaced it") {
		t.Errorf("throttling_came_with_rollout = %+v", came)
	}
	for _, r := range []check.Result{healthy, recent, came} {
		if !strings.Contains(strings.Join(r.EvidenceIDs, " "), fix.ID) {
			t.Errorf("%s cites %v, not Themis's record %s", r.Name, r.EvidenceIDs, fix.ID)
		}
	}
}
