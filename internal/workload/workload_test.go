package workload_test

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/workload"
)

var (
	target = incident.TargetRef{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "d-1"}
	labels = map[string]string{"app": "checkout"}
	at     = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
)

// template is a fixed pod template, whose hash TestTemplateHash pins.
func template(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "checkout"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: image,
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
			Env:       []corev1.EnvVar{{Name: "PORT", Value: "8080"}},
		}}},
	}
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "shop", UID: "d-1", Generation: 4,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "3"},
			Labels:      map[string]string{"argocd.argoproj.io/instance": "shop"},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1", FieldsType: "FieldsV1", Time: ptr.To(metav1.NewTime(at.Add(-time.Hour))), FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:template":{}}}`)},
				{Manager: "argocd-controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1", FieldsType: "FieldsV1", Time: ptr.To(metav1.NewTime(at)), FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:template":{}}}`)},
				{Manager: "kube-controller-manager", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1", FieldsType: "FieldsV1", Subresource: "status", Time: ptr.To(metav1.NewTime(at.Add(time.Hour))), FieldsV1: metav1.NewFieldsV1(`{"f:status":{}}`)},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: template("shop/checkout:1.4.2"),
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2},
	}
}

func replicaSet(name string, uid types.UID, revision, image string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t := template(image)
	t.Labels["pod-template-hash"] = name
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", UID: uid, Labels: labels,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": revision},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: t},
	}
	if owner != nil {
		rs.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	}
	return rs
}

func pod(name string, owner *appsv1.ReplicaSet) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop", Labels: labels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
	}}
}

func reader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithReturnManagedFields().WithObjects(objs...).Build()
}

// A target that is no longer the object the incident is about says exactly
// what happened, so the audit trail can tell deleted from replaced.
func TestRead_TargetLost(t *testing.T) {
	replaced := deployment()
	replaced.UID = "d-2"
	for name, tc := range map[string]struct {
		c      client.Reader
		target incident.TargetRef
		want   error
		msg    string
	}{
		"deleted":  {reader(), target, workload.ErrGone, "Deployment checkout in shop was deleted"},
		"replaced": {reader(replaced), target, workload.ErrReplaced, "Deployment checkout in shop was replaced by a new object with the same name: expected uid d-1, found d-2"},
		"a kind Themis can't act on": {
			reader(),
			incident.TargetRef{Group: "apps", Kind: "StatefulSet", Namespace: "shop", Name: "db", UID: "s-1"},
			workload.ErrUnsupported, "StatefulSet db in shop is not a kind Themis can act on",
		},
	} {
		_, err := workload.Read(t.Context(), tc.c, tc.target)
		if !errors.Is(err, tc.want) || !workload.Lost(err) || err.Error() != tc.msg {
			t.Errorf("%s: err = %v, want %v saying %q", name, err, tc.want, tc.msg)
		}
	}
}

// A read reports facts about the workload as Kubernetes has them; judging
// them is the reader's job.
func TestRead_Facts(t *testing.T) {
	w, err := workload.Read(t.Context(), reader(deployment()), target)
	if err != nil {
		t.Fatal(err)
	}
	if w.Revision != 3 || w.Generation != 4 {
		t.Errorf("revision %d, generation %d, want 3 and 4", w.Revision, w.Generation)
	}
	if w.Writer != "argocd-controller" || !w.WrittenAt.Equal(at) {
		t.Errorf("template written by %q at %v, want the latest template writer, argocd-controller at %v", w.Writer, w.WrittenAt, at)
	}
	if w.ManagedBy != "argocd" {
		t.Errorf("managed by %q, want argocd", w.ManagedBy)
	}
	want := workload.Replicas{Desired: 3, Total: 3, Updated: 3, Ready: 2, Available: 2, Observed: true}
	if w.Replicas != want {
		t.Errorf("replicas %+v, want %+v", w.Replicas, want)
	}
	if w.Object().UID != "d-1" {
		t.Error("Object is not the Deployment read")
	}
}

// A rollout replaces pods as many at a time as its strategy lets it, as
// Kubernetes rounds the strategy's shares, and at least one.
func TestAtOnce(t *testing.T) {
	share := func(s string) *intstr.IntOrString { v := intstr.Parse(s); return &v }
	for name, tc := range map[string]struct {
		replicas int32
		strategy appsv1.DeploymentStrategy
		want     int32
	}{
		"Kubernetes' defaults, 3 pods":  {3, appsv1.DeploymentStrategy{}, 1},
		"Kubernetes' defaults, 10 pods": {10, appsv1.DeploymentStrategy{}, 5},
		"recreated":                     {4, appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, 4},
		"two above, none below":         {6, appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: share("2"), MaxUnavailable: share("0")}}, 2},
		"no more than it runs":          {2, appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: share("100%"), MaxUnavailable: share("100%")}}, 2},
		"neither, still one":            {3, appsv1.DeploymentStrategy{RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: share("0"), MaxUnavailable: share("0")}}, 1},
	} {
		d := deployment()
		d.Spec.Replicas, d.Spec.Strategy = ptr.To(tc.replicas), tc.strategy
		w, err := workload.Read(t.Context(), reader(d), target)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.AtOnce(); got != tc.want {
			t.Errorf("%s: AtOnce = %d, want %d", name, got, tc.want)
		}
	}
}

// Approvals, actions and incidents store the template hash, so its output
// for a given template must never change.
func TestTemplateHash(t *testing.T) {
	h, err := workload.TemplateHash(template("shop/checkout:1.4.2"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:4f493d88f7f3d91ad0d1e22da5a73c4fe5f128206e04584be14acc4410e2d8ed"; h != want {
		t.Errorf("TemplateHash = %s, want %s: stored hashes would no longer match", h, want)
	}
}

// Revisions and pods are the ones the Deployment controls, not whatever
// matches its labels. A pod shutting down belongs to no revision, such as
// one a ReplicaSet the Deployment took up again still stops from its earlier
// use: the current revision's state, which verification reads, leaves its
// restarts out.
func TestRevisionsAndPods(t *testing.T) {
	d := deployment()
	current := replicaSet("checkout-c", "rs-3", "3", "shop/checkout:1.4.2", d)
	previous := replicaSet("checkout-p", "rs-2", "2", "shop/checkout:1.4.1", d)
	stranger := replicaSet("other", "rs-x", "9", "other:1", nil)
	a := pod("a", current)
	a.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1}}
	stopping := pod("s", current)
	stopping.DeletionTimestamp = ptr.To(metav1.NewTime(at))
	// The fake client keeps a deleted object only while a finalizer holds it.
	stopping.Finalizers = []string{"example.com/hold"}
	stopping.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 4}}
	w, err := workload.Read(t.Context(), reader(d, current, previous, stranger, a, stopping, pod("b", previous), pod("c", stranger)), target)
	if err != nil {
		t.Fatal(err)
	}

	revisions, err := w.Revisions(t.Context())
	if err != nil || len(revisions) != 2 {
		t.Fatalf("revisions = %d, %v, want the 2 the Deployment controls", len(revisions), err)
	}
	r, err := w.RevisionAt(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Template.Labels["pod-template-hash"]; ok || r.Template.Spec.Containers[0].Image != "shop/checkout:1.4.1" {
		t.Errorf("revision 2's template = %+v, want 1.4.1 without Kubernetes' per-revision label", r.Template)
	}
	if _, err := w.RevisionAt(t.Context(), 1); !errors.Is(err, workload.ErrNoRevision) {
		t.Errorf("revision 1 = %v, want ErrNoRevision", err)
	}

	pods, err := w.Pods(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, p := range pods {
		got[p.Name] = p.Revision
	}
	if len(got) != 2 || got["a"] != 3 || got["b"] != 2 {
		t.Errorf("pods = %v, want a at revision 3 and b at revision 2, and not s, which is shutting down", got)
	}
	if n, err := w.ShuttingDown(t.Context()); err != nil || n != 1 {
		t.Errorf("shutting down = %d, %v, want 1", n, err)
	}
	if s, err := w.State(t.Context(), at); err != nil || s.Restarts != 1 {
		t.Errorf("state = %+v, %v, want the 1 restart of pod a", s, err)
	}
}

// The executor may list ReplicaSets but not pods, so finding a revision must
// never list pods.
func TestRevisionAt_NeedsNoPods(t *testing.T) {
	d := deployment()
	previous := replicaSet("checkout-p", "rs-2", "2", "shop/checkout:1.4.1", d)
	c := fake.NewClientBuilder().WithObjects(d, previous).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("pods are forbidden")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	w, err := workload.Read(t.Context(), c, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RevisionAt(t.Context(), 2); err != nil {
		t.Errorf("RevisionAt without pod access: %v", err)
	}
}
