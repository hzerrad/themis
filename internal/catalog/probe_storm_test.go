package catalog_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/hzerrad/themis/internal/catalog"
	"github.com/hzerrad/themis/internal/catalog/check"
	"github.com/hzerrad/themis/internal/catalog/evidence"
	"github.com/hzerrad/themis/internal/config"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

// stormStart is when inventory's pods started, in the probe storm below.
var stormStart = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) metav1.Time { return metav1.NewTime(stormStart.Add(d)) }

// stormPod is a pod of inventory as a kind cluster showed it: its liveness
// probe allows 1 second for a health check that takes 2, so the kubelet kills
// each run 20 seconds in and SIGKILLs it 5 seconds later, exit code 137. It
// was restarted 6 times, and its 7th run is being stopped: the kubelet's 7th
// kill is in its events, and its status still shows the 6th run's end.
func stormPod(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: name, UID: uid,
			Labels: map[string]string{"app": "inventory", "pod-template-hash": "67cd7958dd"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "inventory-67cd7958dd", UID: "uid-rs", Controller: new(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "busybox:1.37"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			StartTime:  new(at(0)),
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Image: "docker.io/library/busybox:1.37", Ready: true, RestartCount: 6,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(7*time.Minute + 45*time.Second)}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "Error",
					StartedAt: at(4*time.Minute + 40*time.Second), FinishedAt: at(5*time.Minute + 5*time.Second),
				}},
			}},
		},
	}
}

// stormEvent is one of the kubelet's events about a pod, as Kubernetes keeps
// a repeated one: once, with when it first and last happened and how often.
func stormEvent(pod *corev1.Pod, kind, reason, message string, count int32, first, last time.Duration) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: fmt.Sprintf("%s.%s.%d", pod.Name, reason, first)},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "shop", Name: pod.Name, UID: pod.UID, APIVersion: "v1", FieldPath: "spec.containers{app}",
		},
		Type: kind, Reason: reason, Message: message, Count: count,
		FirstTimestamp: at(first), LastTimestamp: at(last),
		Source: corev1.EventSource{Component: "kubelet", Host: "themis-dev-worker2"},
	}
}

const (
	livenessKill    = "Container app failed liveness probe, will be restarted"
	livenessTimeout = `Liveness probe failed: Get "http://10.244.2.11:8080/cgi-bin/healthz": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`
)

// stormEvents are the kubelet's events about pod in the storm: 7 kills, the
// first 25 seconds in and the last in progress, and 21 probe timeouts, the
// last after the 6th run ended.
func stormEvents(pod *corev1.Pod) []client.Object {
	return []client.Object{
		stormEvent(pod, "Normal", "Killing", livenessKill, 7, 25*time.Second, 8*time.Minute+5*time.Second),
		stormEvent(pod, "Warning", "Unhealthy", livenessTimeout, 21, 15*time.Second, 8*time.Minute+5*time.Second),
	}
}

// diagnoseStorm diagnoses inventory under the probe-restart-storm profile,
// with the pods and events given, 2 seconds into the 7th kill.
func diagnoseStorm(t *testing.T, objects ...client.Object) map[string]check.Result {
	t.Helper()
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Profile("probe-restart-storm")
	if !ok {
		t.Fatal("no probe-restart-storm pack")
	}
	labels := map[string]string{"app": "inventory"}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "busybox:1.37",
			LivenessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/cgi-bin/healthz", Port: intstr.FromInt32(8080)}},
				InitialDelaySeconds: 5, PeriodSeconds: 5, TimeoutSeconds: 1, FailureThreshold: 3, SuccessThreshold: 1,
			},
		}}},
	}
	revision := map[string]string{"deployment.kubernetes.io/revision": "1"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "inventory", UID: "uid-inventory", Annotations: revision, CreationTimestamp: at(-time.Minute)},
		Spec:       appsv1.DeploymentSpec{Replicas: new(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template},
	}
	rsTemplate := *template.DeepCopy()
	rsTemplate.Labels = map[string]string{"app": "inventory", "pod-template-hash": "67cd7958dd"}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "inventory-67cd7958dd", UID: "uid-rs", Annotations: revision, CreationTimestamp: at(-time.Minute),
			Labels: rsTemplate.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "inventory", UID: "uid-inventory", Controller: new(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: new(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: rsTemplate.Labels}, Template: rsTemplate},
	}
	c := fake.NewClientBuilder().WithObjects(append([]client.Object{d, rs}, objects...)...).Build()
	w, err := workload.Read(t.Context(), c, incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "inventory", UID: "uid-inventory"})
	if err != nil {
		t.Fatal(err)
	}
	diag := cat.Diagnose(t.Context(), c, w, config.Matched{p}, catalog.Moment{Now: stormStart.Add(8*time.Minute + 7*time.Second)})
	results := map[string]check.Result{}
	for _, r := range diag.Results {
		results[r.Name] = r
	}
	return results
}

func wantTruths(t *testing.T, got map[string]check.Result, want map[string]check.Truth) {
	t.Helper()
	for name, truth := range want {
		if r := got[name]; r.Value != truth {
			t.Errorf("%s = %s (%s), want %s", name, r.Value, r.Reason, truth)
		}
	}
}

// In a probe storm the probe fails again after every restart, so the
// kubelet's repeated events last happened after the restart the pods' status
// shows. The status bears them out from when they first happened: the checks
// that justify a probe change find the kills and the timeouts.
func TestProbeStorm_RepeatedEventsBorneOutFromTheirFirstTime(t *testing.T) {
	a := stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz")
	b := stormPod("inventory-67cd7958dd-q4n8s", "uid-q4n8s")
	objects := append([]client.Object{a, b}, stormEvents(a)...)
	got := diagnoseStorm(t, append(objects, stormEvents(b)...)...)
	wantTruths(t, got, map[string]check.Truth{
		"probe_kills":           check.True,
		"probe_timeouts":        check.True,
		"killed_during_startup": check.True,
		"crashed_on_its_own":    check.False,
	})
	if r := got["probe_kills"]; r.Reason != "Pod/inventory-67cd7958dd-7fxhz: "+livenessKill+", 7 times" {
		t.Errorf("probe_kills reason = %q", r.Reason)
	}
}

// An event alone never makes a check true: one about a pod whose status
// doesn't show what it says leaves the checks unknown.
func TestProbeStorm_EventAloneIsNotEnough(t *testing.T) {
	never := func() *corev1.Pod {
		p := stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz")
		cs := &p.Status.ContainerStatuses[0]
		cs.RestartCount, cs.LastTerminationState = 0, corev1.ContainerState{}
		cs.State.Running.StartedAt = at(5 * time.Second)
		return p
	}
	crashed := func() *corev1.Pod {
		p := stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz")
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 1
		return p
	}
	tests := []struct {
		name   string
		pod    *corev1.Pod
		events func(*corev1.Pod) []client.Object
	}{
		{"the pod never restarted", never(), stormEvents},
		{"the container ended by itself", crashed(), stormEvents},
		{"its last restart came before the events began", stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz"), func(p *corev1.Pod) []client.Object {
			return []client.Object{
				stormEvent(p, "Normal", "Killing", livenessKill, 1, 6*time.Minute, 6*time.Minute),
				stormEvent(p, "Warning", "Unhealthy", livenessTimeout, 3, 5*time.Minute+50*time.Second, 6*time.Minute),
			}
		}},
		{"the kill names another container", stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz"), func(p *corev1.Pod) []client.Object {
			return []client.Object{
				stormEvent(p, "Normal", "Killing", "Container sidecar failed liveness probe, will be restarted", 7, 25*time.Second, 8*time.Minute),
				stormEvent(p, "Warning", "Unhealthy", livenessTimeout, 21, 15*time.Second, 8*time.Minute),
			}
		}},
		// The pods were killed, and nothing but the timeouts says why.
		{"no kill for a failing probe", stormPod("inventory-67cd7958dd-7fxhz", "uid-7fxhz"), func(p *corev1.Pod) []client.Object {
			return []client.Object{stormEvent(p, "Warning", "Unhealthy", livenessTimeout, 21, 15*time.Second, 8*time.Minute)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diagnoseStorm(t, append([]client.Object{tt.pod}, tt.events(tt.pod)...)...)
			if v := got["probe_kills"].Value; v == check.True {
				t.Errorf("probe_kills = true (%s), want it not true", got["probe_kills"].Reason)
			}
			wantTruths(t, got, map[string]check.Truth{"probe_timeouts": check.Unknown})
		})
	}
}

// Text written in the logs to look like the kubelet's can't reach these
// checks: none of them reads the logs.
func TestProbeStorm_ChecksDontReadLogs(t *testing.T) {
	cat, err := catalog.Themis(nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cat.Profile("probe-restart-storm")
	for _, c := range cat.Checks.For(p.Checks) {
		if slices.Contains(c.Reads, evidence.ContainerLogsTail) {
			t.Errorf("%s reads %s", c.Name, evidence.ContainerLogsTail)
		}
	}
}
