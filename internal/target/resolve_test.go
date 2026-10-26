package target_test

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hzerrad/themis/internal/incident"
	"github.com/hzerrad/themis/internal/target"
	"github.com/hzerrad/themis/internal/testutil/kubetest"
)

func targetOf(group, kind string, obj client.Object) incident.TargetRef {
	return incident.TargetRef{
		Group: group, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()),
	}
}

func resolve(t *testing.T, labels map[string]string) (incident.TargetRef, error) {
	t.Helper()
	return target.Resolve(t.Context(), k8s, k8s.RESTMapper(), labels)
}

func wantTarget(t *testing.T, labels map[string]string, want incident.TargetRef) {
	t.Helper()
	got, err := resolve(t, labels)
	if err != nil {
		t.Fatalf("Resolve(%v): %v", labels, err)
	}
	if got != want {
		t.Errorf("Resolve(%v)\n got  %+v\n want %+v", labels, got, want)
	}
}

func wantErr(t *testing.T, labels map[string]string, want error) {
	t.Helper()
	got, err := resolve(t, labels)
	if !errors.Is(err, want) {
		t.Fatalf("Resolve(%v) = %+v, %v; want error %v", labels, got, err, want)
	}
}

// Walking owners from a Pod.

func TestResolve_PodToDeployment(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d, p := k8s.NewRollout(t, ns, "checkout")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "container": "app"}, targetOf("apps", "Deployment", d))
}

func TestResolve_PodToStatefulSet(t *testing.T) {
	ns := k8s.NewNamespace(t)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
		Spec:       appsv1.StatefulSetSpec{Selector: kubetest.Selector(), Template: kubetest.PodTemplate()},
	}
	k8s.Create(t, sts)
	p := k8s.NewPod(t, ns, "db-0", kubetest.RefTo(sts, "apps/v1", "StatefulSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("apps", "StatefulSet", sts))
}

func TestResolve_PodToDaemonSet(t *testing.T) {
	ns := k8s.NewNamespace(t)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "agent"},
		Spec:       appsv1.DaemonSetSpec{Selector: kubetest.Selector(), Template: kubetest.PodTemplate()},
	}
	k8s.Create(t, ds)
	p := k8s.NewPod(t, ns, "agent-abcde", kubetest.RefTo(ds, "apps/v1", "DaemonSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("apps", "DaemonSet", ds))
}

func TestResolve_PodToJobToCronJob(t *testing.T) {
	ns := k8s.NewNamespace(t)
	jobTemplate := kubetest.PodTemplate()
	jobTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "report"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}},
		},
	}
	k8s.Create(t, cj)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "report-2961", OwnerReferences: []metav1.OwnerReference{kubetest.RefTo(cj, "batch/v1", "CronJob")},
		},
		Spec: batchv1.JobSpec{Template: jobTemplate},
	}
	k8s.Create(t, job)
	p := k8s.NewPod(t, ns, "report-2961-q8z", kubetest.RefTo(job, "batch/v1", "Job"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("batch", "CronJob", cj))
}

func TestResolve_CustomResourceAtTop(t *testing.T) {
	ns := k8s.NewNamespace(t)
	ro := &unstructured.Unstructured{}
	ro.SetAPIVersion("argoproj.io/v1alpha1")
	ro.SetKind("Rollout")
	ro.SetNamespace(ns)
	ro.SetName("checkout")
	k8s.Create(t, ro)
	rs := k8s.NewReplicaSet(t, ns, "checkout-5c8d", kubetest.RefTo(ro, "argoproj.io/v1alpha1", "Rollout"))
	p := k8s.NewPod(t, ns, "checkout-5c8d-k2j", kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("argoproj.io", "Rollout", ro))
}

// An object without a controller is its own target.

func TestResolve_BarePodIsItsOwnTarget(t *testing.T) {
	ns := k8s.NewNamespace(t)
	p := k8s.NewPod(t, ns, "debug")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("", "Pod", p))
}

func TestResolve_BareReplicaSetIsTheTarget(t *testing.T) {
	ns := k8s.NewNamespace(t)
	rs := k8s.NewReplicaSet(t, ns, "standalone")
	p := k8s.NewPod(t, ns, "standalone-abc", kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("apps", "ReplicaSet", rs))
}

// Only the controller owner counts: other owners do not manage the object.
func TestResolve_NonControllerOwnerIgnored(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.RefTo(d, "apps/v1", "Deployment"))
	ref := kubetest.RefTo(rs, "apps/v1", "ReplicaSet")
	ref.Controller = nil
	p := k8s.NewPod(t, ns, "checkout-7d9f-x2k", ref)

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name}, targetOf("", "Pod", p))
}

// Labels that name an object directly.

func TestResolve_ObjectLabels(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
		Spec:       appsv1.StatefulSetSpec{Selector: kubetest.Selector(), Template: kubetest.PodTemplate()},
	}
	k8s.Create(t, sts)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "data-db-0"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	k8s.Create(t, pvc)

	wantTarget(t, map[string]string{"namespace": ns, "deployment": d.Name}, targetOf("apps", "Deployment", d))
	wantTarget(t, map[string]string{"namespace": ns, "statefulset": sts.Name}, targetOf("apps", "StatefulSet", sts))
	wantTarget(t, map[string]string{"namespace": ns, "persistentvolumeclaim": pvc.Name}, targetOf("", "PersistentVolumeClaim", pvc))
}

// Labels that name an object are walked up too: a Job label resolves to the
// CronJob that owns the Job.
func TestResolve_ObjectLabelIsWalkedUp(t *testing.T) {
	ns := k8s.NewNamespace(t)
	jobTemplate := kubetest.PodTemplate()
	jobTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "report"},
		Spec: batchv1.CronJobSpec{
			Schedule:    "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: jobTemplate}},
		},
	}
	k8s.Create(t, cj)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "report-2961", OwnerReferences: []metav1.OwnerReference{kubetest.RefTo(cj, "batch/v1", "CronJob")},
		},
		Spec: batchv1.JobSpec{Template: jobTemplate},
	}
	k8s.Create(t, job)

	wantTarget(t, map[string]string{"namespace": ns, "job_name": job.Name}, targetOf("batch", "CronJob", cj))
}

func TestResolve_WorkloadLabelWinsOverPod(t *testing.T) {
	ns := k8s.NewNamespace(t)
	_, p := k8s.NewRollout(t, ns, "checkout")
	other := k8s.NewDeployment(t, ns, "payments")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "deployment": other.Name}, targetOf("apps", "Deployment", other))
}

// The themis_target override.

func TestResolve_ThemisTargetOverride(t *testing.T) {
	ns := k8s.NewNamespace(t)
	_, p := k8s.NewRollout(t, ns, "checkout")
	api := k8s.NewDeployment(t, ns, "api")

	wantTarget(t, map[string]string{"namespace": ns, "pod": p.Name, "themis_target": "Deployment.apps/api"}, targetOf("apps", "Deployment", api))
}

// The override names the target exactly; its owners are not followed.
func TestResolve_ThemisTargetIsNotWalkedUp(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d := k8s.NewDeployment(t, ns, "checkout")
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.RefTo(d, "apps/v1", "Deployment"))

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "ReplicaSet.apps/" + rs.Name}, targetOf("apps", "ReplicaSet", rs))
}

func TestResolve_ThemisTargetCoreKind(t *testing.T) {
	ns := k8s.NewNamespace(t)
	p := k8s.NewPod(t, ns, "debug")

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "Pod/debug"}, targetOf("", "Pod", p))
}

func TestResolve_ThemisTargetMalformed(t *testing.T) {
	ns := k8s.NewNamespace(t)
	k8s.NewDeployment(t, ns, "api")

	// The kind uses the same Kind.group syntax as profiles. Kinds are
	// UpperCamelCase, so "deployment.apps" is malformed, not a different
	// spelling of the same kind.
	for _, override := range []string{"api", "Deployment.apps/", "/api", "Deployment.apps/api/extra", "deployment.apps/api", "apps/Deployment/api"} {
		got, err := resolve(t, map[string]string{"namespace": ns, "themis_target": override})
		if !errors.Is(err, target.ErrInvalidOverride) {
			t.Errorf("themis_target=%q: got %+v, %v; want ErrInvalidOverride", override, got, err)
		}
	}
}

func TestResolve_ThemisTargetUnknownKind(t *testing.T) {
	ns := k8s.NewNamespace(t)
	k8s.NewDeployment(t, ns, "api")

	for _, override := range []string{"Deploymnt.apps/api", "Deployment.example.com/api"} {
		got, err := resolve(t, map[string]string{"namespace": ns, "themis_target": override})
		if !errors.Is(err, target.ErrUnknownKind) {
			t.Errorf("themis_target=%q: got %+v, %v; want ErrUnknownKind", override, got, err)
		}
	}
}

// Objects that are missing or replaced.

func TestResolve_NotFound(t *testing.T) {
	ns := k8s.NewNamespace(t)

	wantErr(t, map[string]string{"namespace": ns, "pod": "gone"}, target.ErrNotFound)
	wantErr(t, map[string]string{"namespace": ns, "deployment": "gone"}, target.ErrNotFound)
	wantErr(t, map[string]string{"namespace": ns, "themis_target": "Deployment.apps/gone"}, target.ErrNotFound)
}

func TestResolve_OwnerNotFound(t *testing.T) {
	ns := k8s.NewNamespace(t)
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.ControllerRef("apps/v1", "Deployment", "deleted", "00000000-0000-0000-0000-000000000001"))
	p := k8s.NewPod(t, ns, "checkout-7d9f-x2k", kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, target.ErrNotFound)
}

// The Deployment was deleted and recreated under the same name, so the
// ReplicaSet's owner reference points at the old object. The new Deployment is
// not the one these Pods belong to.
func TestResolve_RecreatedOwnerNotFound(t *testing.T) {
	ns := k8s.NewNamespace(t)
	k8s.NewDeployment(t, ns, "checkout")
	rs := k8s.NewReplicaSet(t, ns, "checkout-7d9f", kubetest.ControllerRef("apps/v1", "Deployment", "checkout", "00000000-0000-0000-0000-000000000001"))
	p := k8s.NewPod(t, ns, "checkout-7d9f-x2k", kubetest.RefTo(rs, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, target.ErrNotFound)
}

func TestResolve_WrongNamespace(t *testing.T) {
	ns := k8s.NewNamespace(t)
	other := k8s.NewNamespace(t)
	_, p := k8s.NewRollout(t, ns, "checkout")

	wantErr(t, map[string]string{"namespace": other, "pod": p.Name}, target.ErrNotFound)
}

// Two ReplicaSets that each claim to control the other.
func TestResolve_OwnerCycle(t *testing.T) {
	ns := k8s.NewNamespace(t)
	a := k8s.NewReplicaSet(t, ns, "a")
	b := k8s.NewReplicaSet(t, ns, "b", kubetest.RefTo(a, "apps/v1", "ReplicaSet"))
	a.OwnerReferences = []metav1.OwnerReference{kubetest.RefTo(b, "apps/v1", "ReplicaSet")}
	if err := k8s.Update(t.Context(), a); err != nil {
		t.Fatalf("update a: %v", err)
	}
	p := k8s.NewPod(t, ns, "a-xyz", kubetest.RefTo(a, "apps/v1", "ReplicaSet"))

	wantErr(t, map[string]string{"namespace": ns, "pod": p.Name}, target.ErrOwnerChainTooLong)
}

// Labels that are missing or empty.

func TestResolve_MissingLabels(t *testing.T) {
	ns := k8s.NewNamespace(t)
	_, p := k8s.NewRollout(t, ns, "checkout")

	wantErr(t, map[string]string{"pod": p.Name}, target.ErrMissingLabels)
	wantErr(t, map[string]string{"namespace": ns}, target.ErrMissingLabels)
	wantErr(t, map[string]string{"namespace": ns, "container": "app"}, target.ErrMissingLabels)
	wantErr(t, nil, target.ErrMissingLabels)
	// Prometheus treats an empty label value as an absent label.
	wantErr(t, map[string]string{"namespace": ns, "themis_target": "", "pod": ""}, target.ErrMissingLabels)
}

func TestResolve_EmptyOverrideFallsBackToLabels(t *testing.T) {
	ns := k8s.NewNamespace(t)
	d, p := k8s.NewRollout(t, ns, "checkout")

	wantTarget(t, map[string]string{"namespace": ns, "themis_target": "", "pod": p.Name}, targetOf("apps", "Deployment", d))
}
