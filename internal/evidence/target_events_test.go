package evidence_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hzerrad/themis/internal/evidence"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

func collectTargetEvents(t *testing.T, target incident.TargetRef) evidence.Item {
	t.Helper()
	item, err := evidence.NewTargetEvents().Collect(t.Context(), k8s, target, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return item
}

func TestTargetEvents_IncludesTheTargetAndItsPods(t *testing.T) {
	data := string(collectTargetEvents(t, loadScenario1(t)).Data)
	for _, want := range []string{"ScalingReplicaSet", "BackOff"} {
		if !strings.Contains(data, want) {
			t.Errorf("Data lacks the %s event: %s", want, data)
		}
	}
	if strings.Contains(data, "cart") {
		t.Errorf("Data has an event of another workload: %s", data)
	}
}

func TestTargetEvents_RecordsTimesInUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+1", 60*60)
	t.Cleanup(func() { time.Local = local })

	backOff := objectNamed(t, collectTargetEvents(t, loadScenario1(t)).Data, "BackOff")
	for _, want := range []string{`"2026-10-27T09:01:30Z"`, `"2026-10-27T09:14:05Z"`} {
		if !contains(backOff, want) {
			t.Errorf("the BackOff event lacks %s: %v", want, backOff)
		}
	}
}

type eventTimes struct {
	first, last, eventTime time.Time
}

func newEvent(t *testing.T, ns, reason string, about corev1.ObjectReference, times eventTimes) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("%s.%s", about.Name, strings.ToLower(reason))},
		InvolvedObject: about,
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        reason + " happened",
		Source:         corev1.EventSource{Component: "kubelet"},
	}
	if !times.first.IsZero() {
		ev.FirstTimestamp = metav1.NewTime(times.first)
	}
	if !times.last.IsZero() {
		ev.LastTimestamp = metav1.NewTime(times.last)
	}
	if !times.eventTime.IsZero() {
		ev.EventTime = metav1.NewMicroTime(times.eventTime)
		ev.ReportingController = "kubelet"
		ev.ReportingInstance = "node-1"
		ev.Action = "Restarting"
	}
	k8s.Create(t, ev)
}

func podRef(p *corev1.Pod) corev1.ObjectReference {
	return corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: p.Namespace, Name: p.Name, UID: p.UID}
}

// A zero time must not reach the model as year 1: it is no time at all.
func TestTargetEvents_MissingTimestampsAreLeftOut(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d, pod := k8s.NewRollout(t, ns, "checkout")
	newEvent(t, ns, "NoTimes", podRef(pod), eventTimes{})

	item := collectTargetEvents(t, deploymentTarget(d))
	if obj := objectNamed(t, item.Data, "NoTimes"); contains(obj, "0001-01-01") {
		t.Errorf("a missing time is reported as the zero time: %v", obj)
	}
}

// Events written through events.k8s.io/v1 carry eventTime instead of the
// legacy timestamps.
func TestTargetEvents_UsesEventTime(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d, pod := k8s.NewRollout(t, ns, "checkout")
	newEvent(t, ns, "Killing", podRef(pod), eventTimes{eventTime: time.Date(2026, 10, 27, 9, 2, 3, 0, time.UTC)})

	item := collectTargetEvents(t, deploymentTarget(d))
	if obj := objectNamed(t, item.Data, "Killing"); !contains(obj, "2026-10-27T09:02:03") {
		t.Errorf("the event's eventTime is missing: %v", obj)
	}
}

// An event about an object that was deleted and recreated under the same name
// is not about the target.
func TestTargetEvents_IgnoresEventsOfAnotherObjectWithTheSameName(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d, pod := k8s.NewRollout(t, ns, "checkout")
	old := podRef(pod)
	old.UID = "uid-of-a-deleted-pod"
	newEvent(t, ns, "Stale", old, eventTimes{last: time.Now()})

	if data := string(collectTargetEvents(t, deploymentTarget(d)).Data); strings.Contains(data, "Stale") {
		t.Errorf("Data has an event of another object: %s", data)
	}
}

func TestTargetEvents_BoundsKeepingTheNewest(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.RefTo(d, "apps/v1", "Deployment"))
	pod := k8s.NewPod(t, ns, "checkout-7d9f-x2k", kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	const events = 120
	for i := range events {
		at := start.Add(time.Duration(i) * time.Minute)
		newEvent(t, ns, fmt.Sprintf("Reason%03d", i), podRef(pod), eventTimes{first: at, last: at})
	}

	item := collectTargetEvents(t, deploymentTarget(d))
	if !item.Truncated {
		t.Errorf("Truncated = false for %d events", events)
	}
	data := string(item.Data)
	if !strings.Contains(data, fmt.Sprintf("Reason%03d", events-1)) {
		t.Error("the newest event was dropped")
	}
	if strings.Contains(data, "Reason000") {
		t.Error("the oldest event was kept")
	}
}

func TestTargetEvents_UnsupportedKind(t *testing.T) {
	target := incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db", UID: "u-1"}
	if _, err := evidence.NewTargetEvents().Collect(t.Context(), k8s, target, nil); !errors.Is(err, evidence.ErrUnsupportedTarget) {
		t.Errorf("err = %v, want ErrUnsupportedTarget", err)
	}
}
