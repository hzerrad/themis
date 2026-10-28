package rollback_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/hzerrad/themis/internal/rollback"
)

func template(env ...corev1.EnvVar) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app", Image: "busybox:1.37", Env: env,
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"memory": resource.MustParse("48Mi")}},
	}}}}
}

func find(changes []rollback.Change, field string) (rollback.Change, bool) {
	for _, c := range changes {
		if c.Field == field {
			return c, true
		}
	}
	return rollback.Change{}, false
}

func TestChanges_EnvByNameNeverByValue(t *testing.T) {
	pre := template(corev1.EnvVar{Name: "MODE", Value: "leak"}, corev1.EnvVar{Name: "DB_PASSWORD", Value: "s3cret-a"}, corev1.EnvVar{Name: "OLD", Value: "x"})
	desired := template(corev1.EnvVar{Name: "MODE", Value: "healthy"}, corev1.EnvVar{Name: "DB_PASSWORD", Value: "s3cret-b"}, corev1.EnvVar{Name: "NEW", Value: "y"})

	changes := rollback.Changes(pre, desired)
	for field, want := range map[string][2]string{
		"env MODE":        {"changed", "changed"},
		"env DB_PASSWORD": {"changed", "changed"},
		"env OLD":         {"set", "removed"},
		"env NEW":         {"not set", "set"},
	} {
		c, ok := find(changes, field)
		if !ok || c.Before != want[0] || c.After != want[1] || c.Container != "app" {
			t.Errorf("%s = %+v, %v; want %v", field, c, ok, want)
		}
	}
	for _, c := range changes {
		for _, v := range []string{c.Before, c.After} {
			if strings.Contains(v, "s3cret") || v == "leak" || v == "healthy" {
				t.Errorf("a change shows an env value: %+v", c)
			}
		}
	}
}

func TestChanges_ImageAndResources(t *testing.T) {
	pre, desired := template(), template()
	desired.Spec.Containers[0].Image = "busybox:1.36"
	desired.Spec.Containers[0].Resources.Limits["memory"] = resource.MustParse("256Mi")

	changes := rollback.Changes(pre, desired)
	if c, ok := find(changes, "image"); !ok || c.Before != "busybox:1.37" || c.After != "busybox:1.36" {
		t.Errorf("image = %+v", c)
	}
	if c, ok := find(changes, "limits"); !ok || !strings.Contains(c.Before, "48Mi") || !strings.Contains(c.After, "256Mi") {
		t.Errorf("limits = %+v", c)
	}
}

// Commands carry secrets as often as env does.
func TestChanges_CommandAndArgsHidden(t *testing.T) {
	pre, desired := template(), template()
	pre.Spec.Containers[0].Args = []string{"--token=abc"}
	desired.Spec.Containers[0].Args = []string{"--token=def"}
	c, ok := find(rollback.Changes(pre, desired), "args")
	if !ok || c.Before != "changed" || strings.Contains(c.After, "def") {
		t.Errorf("args = %+v, %v", c, ok)
	}
}

func TestChanges_NothingDiffers(t *testing.T) {
	if changes := rollback.Changes(template(), template()); len(changes) != 0 {
		t.Errorf("Changes = %+v, want none", changes)
	}
}

func TestChanges_ContainerAddedOrRemoved(t *testing.T) {
	pre, desired := template(), template()
	desired.Spec.Containers = append(desired.Spec.Containers, corev1.Container{Name: "sidecar", Image: "envoy:1"})
	if c, ok := find(rollback.Changes(pre, desired), "container"); !ok || c.Container != "sidecar" || c.After != "added" {
		t.Errorf("container = %+v, %v", c, ok)
	}
}

// Every difference is disclosed, even in fields not described one by one.
func TestChanges_ListsEveryOtherDifference(t *testing.T) {
	pre, desired := template(), template()
	desired.Spec.ServiceAccountName = "privileged"
	desired.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	desired.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "busybox"}}

	changes := rollback.Changes(pre, desired)
	for _, field := range []string{"spec.serviceAccountName", "spec.containers.app.securityContext", "spec.initContainers"} {
		if c, ok := find(changes, field); !ok || c.After != "changed" {
			t.Errorf("%s is not disclosed: %+v", field, changes)
		}
	}
}
