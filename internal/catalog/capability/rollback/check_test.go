package rollback_test

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/rollback"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// checked is a rolled-back Deployment at revision 3 with three pods, as
// change leaves it, read through a fake API server.
func checked(t *testing.T, change func(d *appsv1.Deployment, pods []*corev1.Pod)) (client.Reader, incident.TargetRef, string) {
	t.Helper()
	labels := map[string]string{"app": "checkout"}
	revision := map[string]string{"deployment.kubernetes.io/revision": "3"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "d-1", Generation: 2, Annotations: revision},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "checkout:1"}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
	}
	mark, err := workload.TemplateHash(d.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "checkout-3", Namespace: "shop", UID: "rs-3", Labels: labels, Annotations: revision,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	var pods []*corev1.Pod
	for _, name := range []string{"a", "b", "c"} {
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "shop", Labels: labels,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}}},
		})
	}
	objs := []client.Object{rs}
	if change != nil {
		change(d, pods)
	}
	if d.Name != "" {
		objs = append(objs, d)
	}
	for _, p := range pods {
		objs = append(objs, p)
	}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "d-1"}
	return fake.NewClientBuilder().WithObjects(objs...).Build(), target, mark
}

// A rollback cured its symptom when every replica is ready and nothing has
// restarted or run out of memory since: compared with how things were when
// checking began, and in words the timeline shows.
func TestCheck(t *testing.T) {
	since := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	allReady := []byte(`{"ready": 3, "restarts": 0}`)
	for name, tc := range map[string]struct {
		change  func(d *appsv1.Deployment, pods []*corev1.Pod)
		start   []byte
		mark    string
		want    capability.Health
		changed bool
		applied bool
		facts   string
	}{
		"healthy": {start: nil, want: capability.Healthy, applied: true, facts: "3 of 3 pods are ready."},
		"ready, but restarting": {change: func(_ *appsv1.Deployment, pods []*corev1.Pod) {
			pods[0].Status.ContainerStatuses[0].RestartCount = 2
		}, start: allReady, want: capability.Improved, applied: true, facts: "containers restarted 2 times"},
		"fewer ready than at the start": {change: func(d *appsv1.Deployment, _ []*corev1.Pod) {
			d.Status.ReadyReplicas, d.Status.AvailableReplicas = 1, 1
		}, start: allReady, want: capability.Worse, applied: true, facts: "1 of 3 pods are ready, down from 3"},
		"out of memory since": {change: func(_ *appsv1.Deployment, pods []*corev1.Pod) {
			pods[1].Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: metav1.NewTime(since.Add(time.Minute))}
		}, start: allReady, want: capability.Unchanged, applied: true, facts: "ran out of memory once"},
		"not rolled out yet": {change: func(d *appsv1.Deployment, _ []*corev1.Pod) {
			d.Status.UpdatedReplicas = 1
		}, start: allReady, want: capability.Unchanged, applied: false, facts: "3 of 3 pods are ready."},
		"changed by someone else": {start: allReady, mark: "sha256:another", want: capability.Healthy, changed: true, applied: true},
		"deleted": {change: func(d *appsv1.Deployment, _ []*corev1.Pod) {
			d.Name = ""
		}, start: allReady, want: capability.Unchanged, changed: true, facts: "was deleted"},
	} {
		r, target, mark := checked(t, tc.change)
		if tc.mark != "" {
			mark = tc.mark
		}
		got, err := rollback.Capability{}.Check(t.Context(), r, target, mark, tc.start, since)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Health != tc.want || got.Changed != tc.changed || got.Applied != tc.applied || !strings.Contains(got.Facts, tc.facts) {
			t.Errorf("%s: %+v; want %s, changed %v, applied %v, saying %q", name, got, tc.want, tc.changed, tc.applied, tc.facts)
		}
		if tc.start == nil && string(got.Start) != `{"ready":3,"restarts":0}` {
			t.Errorf("%s: start = %s, want how things are now", name, got.Start)
		}
	}
}
