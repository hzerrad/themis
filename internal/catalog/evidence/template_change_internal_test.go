package evidence

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A rollout changes what the pods run, or only the template's labels and
// annotations, which a rollback puts back without changing what runs. A
// value that may be a secret is named, never shown.
func TestTemplateChange(t *testing.T) {
	base := func() corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app", Image: "shop/checkout:1.4",
					Env: []corev1.EnvVar{{Name: "MODE", Value: "healthy"}, {Name: "DB_PASSWORD", Value: "fixture-s3cr3t-do-not-leak"}},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
					},
					LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}}, TimeoutSeconds: 1},
				}},
				Volumes: []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings-v1"}}}}},
			},
		}
	}
	tests := []struct {
		name          string
		edit          func(*corev1.PodTemplateSpec)
		app, metadata []string
	}{
		{"nothing", func(*corev1.PodTemplateSpec) {}, nil, nil},
		{"a restart", func(p *corev1.PodTemplateSpec) {
			p.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": "2026-11-07T10:00:00Z"}
		}, nil, []string{"the annotation kubectl.kubernetes.io/restartedAt was added"}},
		{"a label and a config checksum", func(p *corev1.PodTemplateSpec) {
			p.Labels = map[string]string{"version": "1.5"}
			p.Annotations = map[string]string{"checksum/config": "abc"}
		}, nil, []string{"the label version was added", "the annotation checksum/config was added"}},
		{
			"the image", func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Image = "shop/checkout:1.5" },
			[]string{"the image of container app changed from shop/checkout:1.4 to shop/checkout:1.5"},
			nil,
		},
		{"the environment", func(p *corev1.PodTemplateSpec) {
			c := &p.Spec.Containers[0]
			c.Env[0].Value, c.Env[1].Value = "leak", "another-s3cr3t"
			c.Env = append(c.Env, corev1.EnvVar{Name: "EXTRA", Value: "x"})
		}, []string{
			"the environment variable MODE of container app changed",
			"the environment variable DB_PASSWORD of container app changed",
			"the environment variable EXTRA of container app was added",
		}, nil},
		{"the command, arguments and environment sources", func(p *corev1.PodTemplateSpec) {
			c := &p.Spec.Containers[0]
			c.Command, c.Args = []string{"sh", "-c"}, []string{"--password=fixture-s3cr3t-do-not-leak"}
			c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "env"}}}}
		}, []string{
			"the command of container app changed", "the arguments of container app changed", "the environment sources of container app changed",
		}, nil},
		{"resources", func(p *corev1.PodTemplateSpec) {
			r := &p.Spec.Containers[0].Resources
			r.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("500m")}
			r.Requests = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}
		}, []string{
			"the CPU limit of container app was set to 500m",
			"the memory limit of container app changed from 512Mi to 256Mi",
			"the memory request of container app was set to 128Mi",
		}, nil},
		{"the same quantity, written otherwise", func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("0.5Gi")
		}, nil, nil},
		{"probes", func(p *corev1.PodTemplateSpec) {
			c := &p.Spec.Containers[0]
			c.LivenessProbe.TimeoutSeconds = 3
			c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}}
		}, []string{"the liveness probe of container app changed", "a readiness probe was added to container app"}, nil},
		{"volumes, mounts and other settings", func(p *corev1.PodTemplateSpec) {
			p.Spec.Volumes[0].ConfigMap.Name = "settings-v2"
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/etc/app"}}
			p.Spec.Containers[0].WorkingDir = "/app"
			p.Spec.NodeSelector = map[string]string{"pool": "big"}
		}, []string{
			"the volume mounts of container app changed", "the workingDir of container app changed",
			"the volume config changed", "the pod's nodeSelector changed",
		}, nil},
		{"containers added and removed", func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Name = "server"
			p.Spec.InitContainers = []corev1.Container{{Name: "migrate", Image: "shop/migrate:1"}}
		}, []string{"container server was added", "container app was removed", "init container migrate was added"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			after := base()
			tt.edit(&after)
			app, metadata := templateChange(base(), after)
			if fmt.Sprint(app) != fmt.Sprint(tt.app) || fmt.Sprint(metadata) != fmt.Sprint(tt.metadata) {
				t.Errorf("app = %q, metadata = %q\nwant %q, %q", app, metadata, tt.app, tt.metadata)
			}
			if s := fmt.Sprint(app, metadata); strings.Contains(s, "s3cr3t") || strings.Contains(s, "leak") || strings.Contains(s, "/healthz") {
				t.Errorf("a value shows: %s", s)
			}
		})
	}
}

// Each list is bounded, and so is each change: anyone who may write the
// Deployment chooses its names.
func TestTemplateChange_Bounded(t *testing.T) {
	before, after := corev1.PodTemplateSpec{}, corev1.PodTemplateSpec{Labels: map[string]string{}}
	for i := range 30 {
		after.Labels[fmt.Sprintf("l%02d", i)] = "x"
	}
	after.Spec.Containers = []corev1.Container{{Name: strings.Repeat("c", 300)}}
	app, metadata := templateChange(before, after)
	if len(metadata) != maxChanges || len(app) != 1 || len(app[0]) > maxMessageBytes {
		t.Errorf("%d metadata changes and %q, want %d and one cut to %d bytes", len(metadata), app, maxChanges, maxMessageBytes)
	}
}
