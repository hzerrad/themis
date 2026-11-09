package catalog_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// rolledOut is when checkout went back to the pod template of its revision
// 2, as revision 4, in the test below.
var rolledOut = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func since(d time.Duration) metav1.Time { return metav1.NewTime(rolledOut.Add(d)) }

// checkoutRS is a ReplicaSet of checkout, at revision with the revisions it
// had before.
func checkoutRS(hash, image, revision, before string, created time.Duration) *appsv1.ReplicaSet {
	labels := map[string]string{"app": "checkout", "pod-template-hash": hash}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "checkout-" + hash, UID: types.UID("uid-" + hash), Labels: labels, CreationTimestamp: since(created),
			Annotations: map[string]string{"deployment.kubernetes.io/revision": revision, "deployment.kubernetes.io/revision-history": before},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", UID: "uid-checkout", Controller: new(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
			},
		},
	}
}

// checkoutPod is a pod of rs created at created, whose container restarted
// restarts times, the last run ending at ended, OOMKilled, when it restarted.
func checkoutPod(rs *appsv1.ReplicaSet, name string, created time.Duration, ready bool, restarts int32, ended time.Duration) *corev1.Pod {
	cs := corev1.ContainerStatus{
		Name: "app", Image: rs.Spec.Template.Spec.Containers[0].Image, Ready: ready, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: since(created)}},
	}
	if restarts > 0 {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", StartedAt: since(ended - 10*time.Second), FinishedAt: since(ended),
		}}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: rs.Name + "-" + name, UID: types.UID("uid-" + name), Labels: rs.Spec.Template.Labels, CreationTimestamp: since(created),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
		},
		Spec: rs.Spec.Template.Spec,
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, StartTime: new(since(created)),
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

// backOff is the kubelet's BackOff event about pod, from first to last.
func backOff(pod *corev1.Pod, first, last time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: pod.Name + ".backoff"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: pod.Name, UID: pod.UID, APIVersion: "v1"},
		Type:           "Warning", Reason: "BackOff", Message: "Back-off restarting failed container app", Count: 3,
		FirstTimestamp: since(first), LastTimestamp: since(last),
		Source: corev1.EventSource{Component: "kubelet"},
	}
}

// checkout ran out of memory at revision 2, was rolled back to revision 1's
// template as revision 3, and went back to revision 2's as revision 4.
// Kubernetes took up revision 2's ReplicaSet again, which still stops a pod
// from then: it drains its connections for 5 minutes. That pod belongs to no
// revision, so its failures and events from before the rollout don't pass
// for revision 4's, and the failures started after the rollout.
func TestReusedReplicaSet_PodShuttingDownBelongsToNoRevision(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Profile("rollout-regression")
	if !ok {
		t.Fatal("no rollout-regression pack")
	}

	good := checkoutRS("6d4f8b7c5", "shop/checkout:1.0", "3", "1", -3*time.Hour)
	bad := checkoutRS("7b9c5d6e8", "shop/checkout:1.1", "4", "2", -time.Hour)
	running := checkoutPod(good, "q4n8s", -2*time.Minute, true, 0, 0)
	stopping := checkoutPod(bad, "x2kqp", -time.Hour, false, 3, -3*time.Minute)
	stopping.DeletionTimestamp, stopping.DeletionGracePeriodSeconds = new(since(3*time.Minute)), new(int64(300))
	// The fake client keeps a deleted object only while a finalizer holds it.
	stopping.Finalizers = []string{"example.com/hold"}
	fresh := checkoutPod(bad, "m7v2d", time.Second, false, 1, 40*time.Second)

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "checkout", UID: "uid-checkout", CreationTimestamp: since(-3 * time.Hour),
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1",
				Time: new(since(0)), FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:template":{}}}`),
			}},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "checkout"}},
				Spec:       bad.Spec.Template.Spec,
			},
		},
	}
	objects := []client.Object{d, good, bad, running, stopping, fresh, backOff(stopping, -50*time.Minute, -3*time.Minute), backOff(fresh, 41*time.Second, 80*time.Second)}
	c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(objects...).Build()
	w, err := workload.Read(t.Context(), c, incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-checkout"})
	if err != nil {
		t.Fatal(err)
	}
	diag := cat.Diagnose(t.Context(), c, w, config.Matched{p}, catalog.Moment{Now: rolledOut.Add(90 * time.Second)})

	results := map[string]check.Result{}
	for _, r := range diag.Results {
		results[r.Name] = r
	}
	wantTruths(t, results, map[string]check.Truth{"failure_onset_after_rollout": check.True})
	for _, it := range diag.Items {
		if it.Template != evidence.PodStates {
			continue
		}
		var v evidence.PodStatesView
		if err := json.Unmarshal(it.Data, &v); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, p := range v.Pods {
			got[p.Name] = p.Revision
		}
		if len(got) != 2 || got[fresh.Name] != "4" || got[running.Name] != "3" || v.ShuttingDown != 1 {
			t.Errorf("pods = %v, %d shutting down, want %s at revision 4, %s at revision 3, and 1 shutting down", got, v.ShuttingDown, fresh.Name, running.Name)
		}
	}
}
