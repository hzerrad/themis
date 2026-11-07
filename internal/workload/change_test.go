package workload_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hzerrad/themis/internal/workload"
)

// Putting back every change to what the pods run gives the earlier pod spec
// again, whatever changed; putting back one leaves the others. Each change
// has a key of its own.
func TestChanges_UndoPutsBackWhatChanged(t *testing.T) {
	base := func() corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "checkout"}},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "migrate", Image: "shop/migrate:1"}},
				Containers: []corev1.Container{{
					Name: "app", Image: "shop/checkout:1.4", Command: []string{"serve"},
					Env: []corev1.EnvVar{
						{Name: "MODE", Value: "healthy"},
						{Name: "DB_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url"}}},
						{Name: "PORT", Value: "8080"},
					},
					Resources:     corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}},
					LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}}, TimeoutSeconds: 1},
				}, {Name: "proxy", Image: "envoy:1"}},
				Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings-v1"}}}}},
			},
		}
	}
	after := base()
	c := &after.Spec.Containers[0]
	c.Image, c.Args, c.WorkingDir = "shop/checkout:1.5", []string{"--fast"}, "/app"
	c.Env = []corev1.EnvVar{{Name: "MODE", Value: "strict"}, {Name: "DB_URL", Value: "postgres://db/shop"}, {Name: "EXTRA", Value: "x"}}
	c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("500m")}
	c.Resources.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
	c.LivenessProbe, c.ReadinessProbe = nil, &corev1.Probe{TimeoutSeconds: 3}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/etc/app"}}
	after.Spec.Containers = append(after.Spec.Containers[:1], corev1.Container{Name: "sidecar", Image: "sidecar:2"})
	after.Spec.InitContainers[0].Image = "shop/migrate:2"
	after.Spec.Volumes = append(after.Spec.Volumes, corev1.Volume{Name: "cache"})
	after.Spec.NodeSelector = map[string]string{"pool": "big"}
	after.Labels["version"] = "1.5"

	app, metadata := workload.Changes(base(), after)
	if len(metadata) != 1 || metadata[0].Key != "label.version" {
		t.Errorf("metadata = %+v", metadata)
	}
	keys := map[string]bool{}
	undone := *after.DeepCopy()
	for _, ch := range app {
		if keys[ch.Key] {
			t.Errorf("two changes have the key %s", ch.Key)
		}
		keys[ch.Key] = true
		ch.Undo(&undone, base())
	}
	if !equality.Semantic.DeepEqual(undone.Spec, base().Spec) {
		t.Errorf("putting back every change gave\n%+v\nwant\n%+v", undone.Spec, base().Spec)
	}
	if len(app) != 18 {
		t.Errorf("%d changes, want 18: %v", len(app), keys)
	}

	one := *after.DeepCopy()
	for _, ch := range app {
		if ch.Key == "container.app.env.DB_URL" {
			ch.Undo(&one, base())
		}
	}
	got := one.Spec.Containers[0]
	if got.Env[1].Value != "" || got.Env[1].ValueFrom == nil || got.Image != "shop/checkout:1.5" || got.Env[0].Value != "strict" {
		t.Errorf("putting back DB_URL only gave %+v", got)
	}
}

// What else deploys a workload is read from the marks it leaves.
func TestRead_Deployer(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*metav1.ObjectMeta)
		want string
	}{
		{"none", func(*metav1.ObjectMeta) {}, ""},
		{"a managed-by label", func(m *metav1.ObjectMeta) { m.Labels["app.kubernetes.io/managed-by"] = "Helm" }, "Helm"},
		{"a Helm release", func(m *metav1.ObjectMeta) { m.Annotations["meta.helm.sh/release-name"] = "shop" }, "Helm"},
		{"a GitLab deploy", func(m *metav1.ObjectMeta) { m.Annotations["app.gitlab.com/app"] = "shop" }, "GitLab"},
		{"a field manager", func(m *metav1.ObjectMeta) {
			m.ManagedFields = append(m.ManagedFields, metav1.ManagedFieldsEntry{Manager: "Terraform", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "apps/v1", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:template":{}}}`)})
		}, "Terraform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := deployment()
			d.Labels, d.ManagedFields = map[string]string{}, nil
			tc.edit(&d.ObjectMeta)
			w, err := workload.Read(t.Context(), reader(d), target)
			if err != nil {
				t.Fatal(err)
			}
			if w.Deployer != tc.want {
				t.Errorf("Deployer = %q, want %q", w.Deployer, tc.want)
			}
		})
	}
}
