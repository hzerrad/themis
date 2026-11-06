package probes_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
	"github.com/hzerrad/themis/internal/workload"
)

// deploy creates checkout, two replicas of a container app with a liveness
// and a readiness probe, in a namespace of its own, and returns its target.
func deploy(t *testing.T) incident.TargetRef {
	t.Helper()
	get := func(path string) corev1.ProbeHandler {
		return corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(8080)}}
	}
	template := kubetest.PodTemplate()
	template.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: get("/healthz")}
	template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: get("/ready")}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: k8s.NewNamespace(t), Name: "checkout"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2)), Selector: kubetest.Selector(), Template: template},
	}
	k8s.Create(t, d)
	return incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}
}

func plan(t *testing.T, target incident.TargetRef, params map[string]any) capability.Plan {
	t.Helper()
	p, err := probes.Capability{}.Plan(t.Context(), k8s, target, params)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return p
}

func live(t *testing.T, target incident.TargetRef) *appsv1.Deployment {
	t.Helper()
	return k8s.Deployment(t, target.Namespace, target.Name)
}

// Plan sets the fields asked for on the live probes, and tells policy which
// way each probe moves and what each field was and becomes.
func TestPlan(t *testing.T) {
	target := deploy(t)
	p := plan(t, target, map[string]any{
		"container": "app", "readiness": map[string]any{"timeoutSeconds": 5.0}, "liveness": map[string]any{"periodSeconds": 5.0},
		"restores": "an action's own key, ignored",
	})
	want := map[string]string{
		"container": "app", "liveness": "stricter", "readiness": "more_lenient", "startup": "unchanged",
		"readiness.timeoutSeconds": "5", "readiness.timeoutSeconds.before": "1",
		"liveness.periodSeconds": "5", "liveness.periodSeconds.before": "10",
	}
	if !maps.Equal(p.Facts, want) {
		t.Errorf("Facts = %v, want %v", p.Facts, want)
	}
	if _, ok := p.Params["restores"]; ok || p.Params["container"] != "app" {
		t.Errorf("Params = %v", p.Params)
	}
	// Kubernetes' default rolling update replaces 2 pods one at a time.
	if p.Target.UID != target.UID || p.Target.SpecHash == "" || p.Mark == p.Target.SpecHash || p.BlastRadius["replicas"] != int64(2) || p.BlastRadius["atOnce"] != int64(1) {
		t.Errorf("plan = %+v", p)
	}
	if want := "Changed checkout's liveness and readiness probes."; p.Done != want {
		t.Errorf("Done = %q, want %q", p.Done, want)
	}
}

// Plan refuses a change it can't make, saying why; only the probe's timing
// is its to change.
func TestPlan_Refused(t *testing.T) {
	target := deploy(t)
	other := func(f func(*incident.TargetRef)) incident.TargetRef { t := target; f(&t); return t }
	for name, tc := range map[string]struct {
		target incident.TargetRef
		params map[string]any
		reason string
		noOp   bool
	}{
		"no such container": {target, map[string]any{"container": "web", "liveness": map[string]any{"timeoutSeconds": 2.0}}, "no container named web", false},
		"no such probe":     {target, map[string]any{"container": "app", "startup": map[string]any{"failureThreshold": 30.0}}, "has no startup probe", false},
		"already so":        {target, map[string]any{"container": "app", "readiness": map[string]any{"failureThreshold": 3.0}}, "already have these settings", true},
		"replaced":          {other(func(t *incident.TargetRef) { t.UID = "another" }), map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": 2.0}}, "was replaced", false},
		"deleted":           {other(func(t *incident.TargetRef) { t.Name = "gone" }), map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": 2.0}}, "was deleted", false},
		"not a Deployment":  {other(func(t *incident.TargetRef) { t.Kind = "StatefulSet" }), map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": 2.0}}, "only change the probes of Deployments", false},
	} {
		_, err := probes.Capability{}.Plan(t.Context(), k8s, tc.target, tc.params)
		refusal, ok := errors.AsType[*capability.Refusal](err)
		if !ok || !strings.Contains(refusal.Reason, tc.reason) || refusal.NoOp != tc.noOp {
			t.Errorf("%s: err = %v, want a refusal saying %q", name, err, tc.reason)
		}
	}
	if _, err := (probes.Capability{}).Plan(t.Context(), k8s, target, map[string]any{}); !errors.Is(err, capability.ErrInvalidParams) {
		t.Errorf("no params: err = %v, want ErrInvalidParams", err)
	}
}

// Apply writes the plan, unless it is a trial run or the workload changed
// since it was read: a write since fails the optimistic lock instead of
// being lost.
func TestApply(t *testing.T) {
	params := map[string]any{"container": "app", "readiness": map[string]any{"timeoutSeconds": 5.0}}
	for name, tc := range map[string]struct {
		dryRun, editedSince bool
		written             bool
	}{
		"applied":       {written: true},
		"a trial run":   {dryRun: true},
		"changed since": {editedSince: true},
	} {
		target := deploy(t)
		p := plan(t, target, params)
		if tc.editedSince {
			d := live(t, target)
			d.Spec.Template.Annotations = map[string]string{"edited": "by-a-person"}
			if err := k8s.Update(t.Context(), d); err != nil {
				t.Fatal(err)
			}
		}
		gen, err := probes.Capability{}.Apply(t.Context(), k8s.Client, p, tc.dryRun)
		if tc.editedSince != (err != nil) {
			t.Errorf("%s: Apply: %v", name, err)
		}
		d := live(t, target)
		if written := d.Spec.Template.Spec.Containers[0].ReadinessProbe.TimeoutSeconds == 5; written != tc.written {
			t.Errorf("%s: written = %v, want %v", name, written, tc.written)
		}
		if tc.written && gen != d.Generation {
			t.Errorf("%s: Apply returned generation %d, want %d", name, gen, d.Generation)
		}
	}
	if _, err := (probes.Capability{}).Apply(t.Context(), k8s.Client, capability.Plan{}, true); err == nil {
		t.Error("Apply took a plan that isn't its own")
	}
}

// After the fix, the live template is the one planned; undoing it sets back
// what it changed, leaving the workload as it was, and then there is nothing
// left to undo.
func TestUndo(t *testing.T) {
	target := deploy(t)
	original, err := workload.Read(t.Context(), k8s, target)
	if err != nil {
		t.Fatal(err)
	}
	fix := plan(t, target, map[string]any{"container": "app", "readiness": map[string]any{"timeoutSeconds": 5.0, "successThreshold": 2.0}})
	if _, err := (probes.Capability{}).Apply(t.Context(), k8s.Client, fix, false); err != nil {
		t.Fatal(err)
	}
	if mark, err := (probes.Capability{}).Mark(t.Context(), k8s, target); err != nil || mark != fix.Mark {
		t.Errorf("Mark = %s, %v; want the planned template's %s", mark, err, fix.Mark)
	}

	name, params, err := probes.Capability{}.Undo(t.Context(), k8s, target, fix.Before)
	if err != nil || name != probes.Name {
		t.Fatalf("Undo = %s, %v", name, err)
	}
	want := map[string]any{"container": "app", "readiness": map[string]any{"timeoutSeconds": int64(1), "successThreshold": int64(1)}}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("Undo params = %v, want %v", params, want)
	}
	undo := plan(t, target, params)
	if _, err := (probes.Capability{}).Apply(t.Context(), k8s.Client, undo, false); err != nil {
		t.Fatal(err)
	}
	if mark, _ := (probes.Capability{}).Mark(t.Context(), k8s, target); mark != original.TemplateHash {
		t.Error("the undo left a template other than the one before the fix")
	}

	_, _, err = probes.Capability{}.Undo(t.Context(), k8s, target, fix.Before)
	if refusal, ok := errors.AsType[*capability.Refusal](err); !ok || !refusal.NoOp {
		t.Errorf("nothing to undo: err = %v, want a no-op refusal", err)
	}
	if err := k8s.Delete(t.Context(), live(t, target)); err != nil {
		t.Fatal(err)
	}
	_, _, err = probes.Capability{}.Undo(t.Context(), k8s, target, fix.Before)
	if refusal, ok := errors.AsType[*capability.Refusal](err); !ok || !strings.Contains(refusal.Reason, "deleted") {
		t.Errorf("deleted: err = %v, want a refusal", err)
	}
}
