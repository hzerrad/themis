package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
	"github.com/hzerrad/themis/internal/incident"
)

var fix = resources.Capability{}

// set is params setting one container's values, as kind.resource: value.
func set(container string, values map[string]string) map[string]any {
	kinds := map[string]any{}
	for k, v := range values {
		kind, res, _ := strings.Cut(k, ".")
		if kinds[kind] == nil {
			kinds[kind] = map[string]any{}
		}
		kinds[kind].(map[string]any)[res] = v
	}
	return map[string]any{"containers": map[string]any{container: kinds}}
}

func list(values ...string) corev1.ResourceList {
	l := corev1.ResourceList{}
	for i := 0; i+1 < len(values); i += 2 {
		l[corev1.ResourceName(values[i])] = resource.MustParse(values[i+1])
	}
	return l
}

// checkout is a Deployment in shop with 4 replicas, rolled out one extra pod
// at a time, on nodes of the general pool. app asks for 128Mi of memory,
// limited to 256Mi, and 100m of CPU; sidecar sets nothing.
func checkout() *appsv1.Deployment {
	labels := map[string]string{"app": "checkout"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", UID: "d-1", Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(4)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge: ptr.To(intstr.FromInt32(1)),
			}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				NodeSelector: map[string]string{"pool": "general"},
				Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "shop", Effect: corev1.TaintEffectNoSchedule}},
				Containers: []corev1.Container{
					{Name: "app", Image: "checkout:1", Resources: corev1.ResourceRequirements{
						Requests: list("memory", "128Mi", "cpu", "100m"), Limits: list("memory", "256Mi"),
					}},
					{Name: "sidecar", Image: "envoy:1"},
				},
			}},
		},
	}
}

var target = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "d-1"}

func cluster(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithObjects(append(objs, checkout())...).Build()
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		want   map[string]any
		err    string
	}{
		"kept in canonical form": {
			params: set("app", map[string]string{"limits.memory": "1024Mi", "requests.cpu": "0.5"}),
			want:   set("app", map[string]string{"limits.memory": "1Gi", "requests.cpu": "500m"}),
		},
		"nothing to set":         {params: map[string]any{}, err: "needs only containers"},
		"something else too":     {params: map[string]any{"containers": set("app", map[string]string{"limits.memory": "1Gi"})["containers"], "revision": 1}, err: "needs only containers"},
		"a container with none":  {params: map[string]any{"containers": map[string]any{"app": map[string]any{}}}, err: `container "app" needs requests or limits`},
		"not requests or limits": {params: set("app", map[string]string{"claims.memory": "1Gi"}), err: "only requests and limits"},
		"not memory or CPU":      {params: set("app", map[string]string{"limits.ephemeral-storage": "1Gi"}), err: `only memory and cpu can be set, not "ephemeral-storage"`},
		"not a quantity":         {params: set("app", map[string]string{"limits.memory": "lots"}), err: "the memory limit lots is not a Kubernetes quantity"},
		"a number":               {params: map[string]any{"containers": map[string]any{"app": map[string]any{"limits": map[string]any{"cpu": 2.0}}}}, err: "is not a Kubernetes quantity"},
		"zero":                   {params: set("app", map[string]string{"limits.memory": "0"}), err: "the memory limit must be more than zero"},
	} {
		got, err := fix.Validate(tc.params)
		if tc.err != "" {
			if !errors.Is(err, capability.ErrInvalidParams) || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", name, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Validate = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// Plan works the change out against the live workload, and refuses one that
// can't go ahead, saying why.
func TestPlan(t *testing.T) {
	for name, tc := range map[string]struct {
		params  map[string]any
		uid     string
		facts   map[string]string
		done    string
		refusal string
		noOp    bool
	}{
		"raises the memory limit": {
			params: set("app", map[string]string{"limits.memory": "512Mi"}),
			facts: map[string]string{
				"containers": "app", "memory": "increase", "memory.ratio": "2",
				"app.memory.limit": "increase", "app.memory.limit.before": "256Mi", "app.memory.limit.after": "512Mi", "app.memory.limit.ratio": "2",
			},
			done: "Raised the memory limit of checkout's app container from 256Mi to 512Mi.",
		},
		"sets one CPU value and lowers another": {
			params: set("app", map[string]string{"limits.cpu": "1", "requests.cpu": "50m"}),
			facts: map[string]string{
				"containers": "app", "cpu": "mixed", "cpu.ratio": "0.5",
				"app.cpu.request": "decrease", "app.cpu.request.before": "100m", "app.cpu.request.after": "50m", "app.cpu.request.ratio": "0.5",
				"app.cpu.limit": "set", "app.cpu.limit.before": "not set", "app.cpu.limit.after": "1",
			},
			done: "Changed the CPU request and CPU limit of checkout's app container.",
		},
		"no such container": {
			params:  set("web", map[string]string{"limits.memory": "512Mi"}),
			refusal: "The workload has no container named web. Its containers are app, sidecar.",
		},
		"a request above its limit": {
			params:  set("app", map[string]string{"requests.memory": "512Mi"}),
			refusal: "app would have a memory request of 512Mi, above its memory limit of 256Mi; Kubernetes doesn't allow a request above its limit.",
		},
		"already so": {
			params:  set("app", map[string]string{"limits.memory": "0.25Gi"}),
			refusal: "Nothing to change. The workload already has these resources.", noOp: true,
		},
		"replaced": {
			params: set("app", map[string]string{"limits.memory": "512Mi"}), uid: "d-0",
			refusal: "The workload was replaced by a new object with the same name, so the fix no longer applies.",
		},
	} {
		tt := target
		if tc.uid != "" {
			tt.UID = tc.uid
		}
		p, err := fix.Plan(t.Context(), cluster(), tt, tc.params)
		if tc.refusal != "" {
			if r, ok := errors.AsType[*capability.Refusal](err); !ok || r.Reason != tc.refusal || r.NoOp != tc.noOp {
				t.Errorf("%s: err = %v, want the refusal %q", name, err, tc.refusal)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !maps.Equal(p.Facts, tc.facts) || p.Done != tc.done {
			t.Errorf("%s: facts %v, done %q; want %v, %q", name, p.Facts, p.Done, tc.facts, tc.done)
		}
		if p.Target.UID != "d-1" || p.Target.SpecHash == "" || p.Mark == p.Target.SpecHash || p.Risk != "medium" || p.BlastRadius["replicas"] != int64(4) {
			t.Errorf("%s: target %+v, mark %s, risk %s, blast radius %v", name, p.Target, p.Mark, p.Risk, p.BlastRadius)
		}
	}
	if _, err := fix.Plan(t.Context(), cluster(), target, map[string]any{}); !errors.Is(err, capability.ErrInvalidParams) {
		t.Errorf("no params: err = %v, want ErrInvalidParams", err)
	}
}

func node(name, memory string, change func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"pool": "general"}},
		Status:     corev1.NodeStatus{Allocatable: list("memory", memory, "cpu", "4")},
	}
	if change != nil {
		change(n)
	}
	return n
}

func limitRange(item corev1.LimitRangeItem) *corev1.LimitRange {
	return &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "limits", Namespace: "shop"}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{item}}}
}

func quota(scopes ...corev1.ResourceQuotaScope) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "shop"},
		Spec:       corev1.ResourceQuotaSpec{Hard: list("requests.memory", "1Gi"), Scopes: scopes},
		Status:     corev1.ResourceQuotaStatus{Used: list("requests.memory", "512Mi")},
	}
}

// A change the cluster can't run is refused, saying which constraint and by
// how much; what Themis can't see doesn't refuse it.
func TestFeasible(t *testing.T) {
	big := set("app", map[string]string{"requests.memory": "6Gi", "limits.memory": "6Gi"})
	for name, tc := range map[string]struct {
		objs    []client.Object
		params  map[string]any
		forbid  bool
		refusal string
	}{
		"fits": {objs: []client.Object{node("n1", "8Gi", nil)}, params: big},
		"no node fits": {
			objs: []client.Object{node("n1", "4Gi", nil), node("n2", "2Gi", nil)}, params: big,
			refusal: "No node can fit a pod with a 6Gi memory request. The largest has 4Gi allocatable.",
		},
		"only nodes the pods can run on count": {
			objs: []client.Object{
				node("small", "4Gi", nil),
				node("cordoned", "16Gi", func(n *corev1.Node) { n.Spec.Unschedulable = true }),
				node("gpu", "16Gi", func(n *corev1.Node) { n.Labels["pool"] = "gpu" }),
				node("tainted", "16Gi", func(n *corev1.Node) {
					n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "batch", Effect: corev1.TaintEffectNoSchedule}}
				}),
			},
			params:  big,
			refusal: "No node can fit a pod with a 6Gi memory request. The largest has 4Gi allocatable.",
		},
		"a tolerated taint": {objs: []client.Object{node("shop", "8Gi", func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "shop", Effect: corev1.TaintEffectNoSchedule}}
		})}, params: big},
		"no nodes Themis can see": {params: big},
		"over the quota": {
			objs: []client.Object{quota()}, params: set("app", map[string]string{"requests.memory": "256Mi"}),
			refusal: "The ResourceQuota compute in shop allows 1Gi of memory requests. 512Mi is in use, and rolling out the change needs 640Mi more.",
		},
		"a quota for some pods only": {objs: []client.Object{quota(corev1.ResourceQuotaScopeBestEffort)}, params: set("app", map[string]string{"requests.memory": "256Mi"})},
		"above a container maximum": {
			objs: []client.Object{limitRange(corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Max: list("memory", "1Gi")})}, params: set("app", map[string]string{"limits.memory": "2Gi"}),
			refusal: "The LimitRange limits in shop allows at most 1Gi of memory per container, and app would have a memory limit of 2Gi.",
		},
		"below a container minimum": {
			objs: []client.Object{limitRange(corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Min: list("cpu", "200m")})}, params: set("app", map[string]string{"requests.cpu": "150m"}),
			refusal: "The LimitRange limits in shop allows no less than 200m of CPU per container, and app would have a CPU request of 150m.",
		},
		"limit too far above the request": {
			objs: []client.Object{limitRange(corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, MaxLimitRequestRatio: list("memory", "2")})}, params: set("app", map[string]string{"limits.memory": "1Gi"}),
			refusal: "The LimitRange limits in shop allows a memory limit at most 2 times the request, and app would have 1Gi for a 128Mi request.",
		},
		"a default limit below the request": {
			objs: []client.Object{limitRange(corev1.LimitRangeItem{Type: corev1.LimitTypeContainer, Default: list("memory", "64Mi")})}, params: set("sidecar", map[string]string{"requests.memory": "128Mi"}),
			refusal: "The LimitRange limits in shop would leave sidecar with a memory request of 128Mi, above its memory limit of 64Mi.",
		},
		"above a pod maximum": {
			objs: []client.Object{limitRange(corev1.LimitRangeItem{Type: corev1.LimitTypePod, Max: list("memory", "512Mi")})}, params: set("app", map[string]string{"limits.memory": "1Gi"}),
			refusal: "The LimitRange limits in shop allows at most 512Mi of memory per pod, and each pod would have a memory limit of 1Gi.",
		},
		"not allowed to read nodes": {
			params: big, forbid: true,
			refusal: "Themis can't tell whether the cluster can run this change. It isn't allowed to read nodes.",
		},
	} {
		var r client.Reader = cluster(tc.objs...)
		if tc.forbid {
			r = interceptor.NewClient(cluster(tc.objs...), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				if _, ok := l.(*corev1.NodeList); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no"))
				}
				return c.List(ctx, l, opts...)
			}})
		}
		p, err := fix.Plan(t.Context(), r, target, tc.params)
		if err != nil {
			t.Fatalf("%s: Plan: %v", name, err)
		}
		err = fix.Feasible(t.Context(), r, p)
		if tc.refusal == "" {
			if err != nil {
				t.Errorf("%s: %v, want it feasible", name, err)
			}
			continue
		}
		if r, ok := errors.AsType[*capability.Refusal](err); !ok || r.Reason != tc.refusal {
			t.Errorf("%s: err = %v, want the refusal %q", name, err, tc.refusal)
		}
	}
}

func TestChanges(t *testing.T) {
	before := checkout().Spec.Template
	after := *before.DeepCopy()
	after.Spec.Containers[0].Resources.Limits["memory"] = resource.MustParse("512Mi")
	after.Spec.Containers[1].Resources.Requests = list("cpu", "100m")
	for name, tc := range map[string]struct {
		after corev1.PodTemplateSpec
		want  []capability.Change
	}{
		"per container, old to new": {after: after, want: []capability.Change{
			{Container: "app", Field: "memory limit", Before: "256Mi", After: "512Mi"},
			{Container: "sidecar", Field: "CPU request", Before: "not set", After: "100m"},
		}},
		"nothing differs": {after: before},
	} {
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(tc.after)
		got, err := fix.Changes(b, a)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Changes = %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

// Undo can't put back what the fix can't write, and says so.
func TestUndo_Refused(t *testing.T) {
	without := checkout().Spec.Template
	without.Spec.Containers[0].Resources.Limits = nil
	for name, tc := range map[string]struct {
		before corev1.PodTemplateSpec
		uid    string
		reason string
		noOp   bool
	}{
		"a value there wasn't": {before: without, reason: "Not restored. Container app had no memory limit before, and Themis never removes one. The fix stays in place."},
		"nothing differs":      {before: checkout().Spec.Template, reason: "Nothing to restore. The workload's resources are already as they were before Themis acted.", noOp: true},
		"replaced":             {before: checkout().Spec.Template, uid: "d-0", reason: "Not restored. The workload was replaced by a new object with the same name."},
	} {
		tt := target
		if tc.uid != "" {
			tt.UID = tc.uid
		}
		before, _ := json.Marshal(tc.before)
		_, _, err := fix.Undo(t.Context(), cluster(), tt, before)
		if r, ok := errors.AsType[*capability.Refusal](err); !ok || r.Reason != tc.reason || r.NoOp != tc.noOp {
			t.Errorf("%s: err = %v, want the refusal %q", name, err, tc.reason)
		}
	}
}
