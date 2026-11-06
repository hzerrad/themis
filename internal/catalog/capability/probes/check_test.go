package probes_test

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
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// checked is a Deployment at revision 2 with two pods after its probes were
// changed, as change leaves it, read through a fake API server.
func checked(t *testing.T, change func(d *appsv1.Deployment, pods []*corev1.Pod)) (client.Reader, incident.TargetRef, string) {
	t.Helper()
	labels := map[string]string{"app": "checkout"}
	revision := map[string]string{"deployment.kubernetes.io/revision": "2"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "d-1", Generation: 2, Annotations: revision},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app", Image: "checkout:1", LivenessProbe: &corev1.Probe{TimeoutSeconds: 5},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	mark, err := workload.TemplateHash(d.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "checkout-2", Namespace: "shop", UID: "rs-2", Labels: labels, Annotations: revision,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	var pods []*corev1.Pod
	for _, name := range []string{"a", "b"} {
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "shop", Labels: labels,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}}},
		})
	}
	if change != nil {
		change(d, pods)
	}
	objs := []client.Object{rs}
	if d.Name != "" {
		objs = append(objs, d)
	}
	for _, p := range pods {
		objs = append(objs, p)
	}
	target := incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "d-1"}
	return fake.NewClientBuilder().WithObjects(objs...).Build(), target, mark
}

// The probes are right when every replica is ready and no container has
// restarted since checking began, as a liveness probe failing a working
// container would make it: compared with how things were when checking
// began, and in words the timeline shows.
func TestCheck(t *testing.T) {
	since := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	allReady := []byte(`{"ready": 2, "restarts": 0}`)
	for name, tc := range map[string]struct {
		change  func(d *appsv1.Deployment, pods []*corev1.Pod)
		start   []byte
		mark    string
		want    capability.Health
		changed bool
		applied bool
		facts   string
	}{
		"healthy": {start: nil, want: capability.Healthy, applied: true, facts: "2 of 2 pods are ready."},
		"ready, but restarted once": {change: func(_ *appsv1.Deployment, pods []*corev1.Pod) {
			pods[0].Status.ContainerStatuses[0].RestartCount = 1
		}, start: allReady, want: capability.Improved, applied: true, facts: "Containers restarted once"},
		"fewer ready than at the start": {change: func(d *appsv1.Deployment, pods []*corev1.Pod) {
			d.Status.ReadyReplicas, d.Status.AvailableReplicas = 0, 0
			pods[1].Status.ContainerStatuses[0].RestartCount = 4
		}, start: allReady, want: capability.Worse, applied: true, facts: "0 of 2 pods are ready, down from 2. Containers restarted 4 times"},
		"not rolled out yet": {change: func(d *appsv1.Deployment, _ []*corev1.Pod) {
			d.Status.UpdatedReplicas = 1
		}, start: allReady, want: capability.Unchanged, facts: "2 of 2 pods are ready."},
		"changed by someone else": {start: allReady, mark: "sha256:another", want: capability.Healthy, changed: true, applied: true},
		"deleted": {change: func(d *appsv1.Deployment, _ []*corev1.Pod) {
			d.Name = ""
		}, start: allReady, want: capability.Unchanged, changed: true, facts: "was deleted"},
	} {
		r, target, mark := checked(t, tc.change)
		if tc.mark != "" {
			mark = tc.mark
		}
		got, err := probes.Capability{}.Check(t.Context(), r, target, mark, tc.start, since)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Health != tc.want || got.Changed != tc.changed || got.Applied != tc.applied || !strings.Contains(got.Facts, tc.facts) {
			t.Errorf("%s: %+v; want %s, changed %v, applied %v, saying %q", name, got, tc.want, tc.changed, tc.applied, tc.facts)
		}
		if tc.start == nil && string(got.Start) != `{"ready":2,"restarts":0}` {
			t.Errorf("%s: start = %s, want how things are now", name, got.Start)
		}
	}
}
