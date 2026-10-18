package intake_test

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/intake"
)

// envtest runs no controllers, so the ReplicaSets, Pods and Jobs that
// controllers would normally create, and their owner references, are created
// by hand.

var podLabels = map[string]string{"app": "checkout"}

func podSpec() corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyAlways,
		Containers:    []corev1.Container{{Name: "app", Image: "example/app:1"}},
	}
}

func podTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: podLabels}, Spec: podSpec()}
}

func selector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: podLabels}
}

func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func controllerRef(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: apiVersion, Kind: kind, Name: name, UID: uid,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

func refTo(obj client.Object, apiVersion, kind string) metav1.OwnerReference {
	return controllerRef(apiVersion, kind, obj.GetName(), obj.GetUID())
}

func newDeployment(t *testing.T, ns, name string) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: selector(), Template: podTemplate()},
	}
	create(t, d)
	return d
}

func newReplicaSet(t *testing.T, ns, name string, owners ...metav1.OwnerReference) *appsv1.ReplicaSet {
	t.Helper()
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec:       appsv1.ReplicaSetSpec{Selector: selector(), Template: podTemplate()},
	}
	create(t, rs)
	return rs
}

func newPod(t *testing.T, ns, name string, owners ...metav1.OwnerReference) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: podLabels, OwnerReferences: owners},
		Spec:       podSpec(),
	}
	create(t, p)
	return p
}

// newRollout creates Deployment → ReplicaSet → Pod, as the Deployment
// controller would.
func newRollout(t *testing.T, ns, name string) (*appsv1.Deployment, *corev1.Pod) {
	t.Helper()
	d := newDeployment(t, ns, name)
	rs := newReplicaSet(t, ns, name+"-7d9f", refTo(d, "apps/v1", "Deployment"))
	p := newPod(t, ns, name+"-7d9f-x2k", refTo(rs, "apps/v1", "ReplicaSet"))
	return d, p
}

func target(group, kind string, obj client.Object) incident.TargetRef {
	return incident.TargetRef{
		Group: group, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()),
	}
}

func resolve(t *testing.T, labels map[string]string) (incident.TargetRef, error) {
	t.Helper()
	return intake.ResolveTarget(t.Context(), k8s, k8s.RESTMapper(), labels)
}

func wantTarget(t *testing.T, labels map[string]string, want incident.TargetRef) {
	t.Helper()
	got, err := resolve(t, labels)
	if err != nil {
		t.Fatalf("ResolveTarget(%v): %v", labels, err)
	}
	if got != want {
		t.Errorf("ResolveTarget(%v)\n got  %+v\n want %+v", labels, got, want)
	}
}

func wantErr(t *testing.T, labels map[string]string, want error) {
	t.Helper()
	got, err := resolve(t, labels)
	if !errors.Is(err, want) {
		t.Fatalf("ResolveTarget(%v) = %+v, %v; want error %v", labels, got, err, want)
	}
}

// Walking owners from a Pod.

func TestResolve_PodToDeployment(t *testing.T) {
	ns := newNamespace(t)
	d, p := newRollout(t, ns, "checkout")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "container": "app"}, target("apps", "Deployment", d))
}

func TestResolve_PodToStatefulSet(t *testing.T) {
	ns := newNamespace(t)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
		Spec:       appsv1.StatefulSetSpec{Selector: selector(), Template: podTemplate()},
	}
	create(t, sts)
	p := newPod(t, ns, "db-0", refTo(sts, "apps/v1", "StatefulSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("apps", "StatefulSet", sts))
}

func TestResolve_PodToDaemonSet(t *testing.T) {
	ns := newNamespace(t)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "agent"},
		Spec:       appsv1.DaemonSetSpec{Selector: selector(), Template: podTemplate()},
	}
	create(t, ds)
	p := newPod(t, ns, "agent-abcde", refTo(ds, "apps/v1", "DaemonSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("apps", "DaemonSet", ds))
}

func TestResolve_PodToJobToCronJob(t *testing.T) {
	ns := newNamespace(t)
	jobTemplate := podTemplate()
	jobTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "report"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}},
		},
	}
	create(t, cj)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "report-2961", OwnerReferences: []metav1.OwnerReference{refTo(cj, "batch/v1", "CronJob")},
		},
		Spec: batchv1.JobSpec{Template: jobTemplate},
	}
	create(t, job)
	p := newPod(t, ns, "report-2961-q8z", refTo(job, "batch/v1", "Job"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("batch", "CronJob", cj))
}

func TestResolve_CustomResourceAtTop(t *testing.T) {
	ns := newNamespace(t)
	ro := &unstructured.Unstructured{}
	ro.SetAPIVersion("argoproj.io/v1alpha1")
	ro.SetKind("Rollout")
	ro.SetNamespace(ns)
	ro.SetName("checkout")
	create(t, ro)
	rs := newReplicaSet(t, ns, "checkout-5c8d", refTo(ro, "argoproj.io/v1alpha1", "Rollout"))
	p := newPod(t, ns, "checkout-5c8d-k2j", refTo(rs, "apps/v1", "ReplicaSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("argoproj.io", "Rollout", ro))
}

// An object without a controller is its own target.

func TestResolve_BarePodIsItsOwnTarget(t *testing.T) {
	ns := newNamespace(t)
	p := newPod(t, ns, "debug")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("", "Pod", p))
}

func TestResolve_BareReplicaSetIsTheTarget(t *testing.T) {
	ns := newNamespace(t)
	rs := newReplicaSet(t, ns, "standalone")
	p := newPod(t, ns, "standalone-abc", refTo(rs, "apps/v1", "ReplicaSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("apps", "ReplicaSet", rs))
}

// Only the controller owner counts: other owners do not manage the object.
func TestResolve_NonControllerOwnerIgnored(t *testing.T) {
	ns := newNamespace(t)
	d := newDeployment(t, ns, "checkout")
	rs := newReplicaSet(t, ns, "checkout-7d9f", refTo(d, "apps/v1", "Deployment"))
	ref := refTo(rs, "apps/v1", "ReplicaSet")
	ref.Controller = nil
	p := newPod(t, ns, "checkout-7d9f-x2k", ref)

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, target("", "Pod", p))
}

// Labels that name an object directly.

func TestResolve_ObjectLabels(t *testing.T) {
	ns := newNamespace(t)
	d := newDeployment(t, ns, "checkout")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
		Spec:       appsv1.StatefulSetSpec{Selector: selector(), Template: podTemplate()},
	}
	create(t, sts)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "data-db-0"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	create(t, pvc)

	wantTarget(t, map[string]string{"namespace": ns, "deployment": d.Name}, target("apps", "Deployment", d))
	wantTarget(t, map[string]string{"namespace": ns, "statefulset": sts.Name}, target("apps", "StatefulSet", sts))
	wantTarget(t, map[string]string{"namespace": ns, "persistentvolumeclaim": pvc.Name}, target("", "PersistentVolumeClaim", pvc))
}

// Labels that name an object are walked up too: a Job label resolves to the
// CronJob that owns the Job.
func TestResolve_ObjectLabelIsWalkedUp(t *testing.T) {
	ns := newNamespace(t)
	jobTemplate := podTemplate()
	jobTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "report"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}},
		},
	}
	create(t, cj)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "report-2961", OwnerReferences: []metav1.OwnerReference{refTo(cj, "batch/v1", "CronJob")},
		},
		Spec: batchv1.JobSpec{Template: jobTemplate},
	}
	create(t, job)

	wantTarget(t, map[string]string{"namespace": ns, "job_name": job.Name}, target("batch", "CronJob", cj))
}

func TestResolve_WorkloadLabelWinsOverPod(t *testing.T) {
	ns := newNamespace(t)
	_, p := newRollout(t, ns, "checkout")
	other := newDeployment(t, ns, "payments")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "deployment": other.Name}, target("apps", "Deployment", other))
}

// The themis_target override.

func TestResolve_ThemisTargetOverride(t *testing.T) {
	ns := newNamespace(t)
	_, p := newRollout(t, ns, "checkout")
	api := newDeployment(t, ns, "api")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "themis_target": "Deployment.apps/api"}, target("apps", "Deployment", api))
}

// The override names the target exactly; its owners are not followed.
func TestResolve_ThemisTargetIsNotWalkedUp(t *testing.T) {
	ns := newNamespace(t)
	d := newDeployment(t, ns, "checkout")
	rs := newReplicaSet(t, ns, "checkout-7d9f", refTo(d, "apps/v1", "Deployment"))

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "ReplicaSet.apps/" + rs.Name}, target("apps", "ReplicaSet", rs))
}

func TestResolve_ThemisTargetCoreKind(t *testing.T) {
	ns := newNamespace(t)
	p := newPod(t, ns, "debug")

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "Pod/debug"}, target("", "Pod", p))
}

func TestResolve_ThemisTargetMalformed(t *testing.T) {
	ns := newNamespace(t)
	newDeployment(t, ns, "api")

	// The kind uses the same Kind.group syntax as profiles. Kinds are
	// UpperCamelCase, so "deployment.apps" is malformed, not a different
	// spelling of the same kind.
	for _, override := range []string{"api", "Deployment.apps/", "/api", "Deployment.apps/api/extra", "deployment.apps/api", "apps/Deployment/api"} {
		got, err := resolve(t, map[string]string{"namespace": ns, "themis_target": override})
		if !errors.Is(err, intake.ErrInvalidOverride) {
			t.Errorf("themis_target=%q: got %+v, %v; want ErrInvalidOverride", override, got, err)
		}
	}
}

func TestResolve_ThemisTargetUnknownKind(t *testing.T) {
	ns := newNamespace(t)
	newDeployment(t, ns, "api")

	for _, override := range []string{"Deploymnt.apps/api", "Deployment.example.com/api"} {
		got, err := resolve(t, map[string]string{"namespace": ns, "themis_target": override})
		if !errors.Is(err, intake.ErrUnknownKind) {
			t.Errorf("themis_target=%q: got %+v, %v; want ErrUnknownKind", override, got, err)
		}
	}
}

// Objects that are missing or replaced.

func TestResolve_NotFound(t *testing.T) {
	ns := newNamespace(t)

	wantErr(t, map[string]string{"namespace": ns, "pod": "gone"}, intake.ErrNotFound)
	wantErr(t, map[string]string{"namespace": ns, "deployment": "gone"}, intake.ErrNotFound)
	wantErr(t, map[string]string{"namespace": ns, "themis_target": "Deployment.apps/gone"}, intake.ErrNotFound)
}

func TestResolve_OwnerNotFound(t *testing.T) {
	ns := newNamespace(t)
	rs := newReplicaSet(t, ns, "checkout-7d9f", controllerRef("apps/v1", "Deployment", "deleted", "00000000-0000-0000-0000-000000000001"))
	p := newPod(t, ns, "checkout-7d9f-x2k", refTo(rs, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, intake.ErrNotFound)
}

// The Deployment was deleted and recreated under the same name, so the
// ReplicaSet's owner reference points at the old object. The new Deployment is
// not the one these Pods belong to.
func TestResolve_RecreatedOwnerNotFound(t *testing.T) {
	ns := newNamespace(t)
	newDeployment(t, ns, "checkout")
	rs := newReplicaSet(t, ns, "checkout-7d9f", controllerRef("apps/v1", "Deployment", "checkout", "00000000-0000-0000-0000-000000000001"))
	p := newPod(t, ns, "checkout-7d9f-x2k", refTo(rs, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, intake.ErrNotFound)
}

func TestResolve_WrongNamespace(t *testing.T) {
	ns := newNamespace(t)
	other := newNamespace(t)
	_, p := newRollout(t, ns, "checkout")

	wantErr(t, map[string]string{"namespace": other, "pod": p.Name}, intake.ErrNotFound)
}

// Two ReplicaSets that each claim to control the other.
func TestResolve_OwnerCycle(t *testing.T) {
	ns := newNamespace(t)
	a := newReplicaSet(t, ns, "a")
	b := newReplicaSet(t, ns, "b", refTo(a, "apps/v1", "ReplicaSet"))
	a.OwnerReferences = []metav1.OwnerReference{refTo(b, "apps/v1", "ReplicaSet")}
	if err := k8s.Update(t.Context(), a); err != nil {
		t.Fatalf("update a: %v", err)
	}
	p := newPod(t, ns, "a-xyz", refTo(a, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, intake.ErrOwnerChainTooLong)
}

// Labels that are missing or empty.

func TestResolve_MissingLabels(t *testing.T) {
	ns := newNamespace(t)
	_, p := newRollout(t, ns, "checkout")

	wantErr(t, map[string]string{"pod": p.Name}, intake.ErrMissingLabels)
	wantErr(t, map[string]string{"namespace": ns}, intake.ErrMissingLabels)
	wantErr(t, map[string]string{"namespace": ns, "container": "app"}, intake.ErrMissingLabels)
	wantErr(t, nil, intake.ErrMissingLabels)
	// Prometheus treats an empty label value as an absent label.
	wantErr(t, map[string]string{"namespace": ns, "themis_target": "", "pod": ""}, intake.ErrMissingLabels)
}

func TestResolve_EmptyOverrideFallsBackToLabels(t *testing.T) {
	ns := newNamespace(t)
	d, p := newRollout(t, ns, "checkout")

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "", "pod": p.Name}, target("apps", "Deployment", d))
}
