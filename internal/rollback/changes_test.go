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

func TestChanges_EnvShownUnlessSecret(t *testing.T) {
	pre := template(
		corev1.EnvVar{Name: "MODE", Value: "leak"},
		corev1.EnvVar{Name: "DB_PASSWORD", Value: "s3cret-a"},
		corev1.EnvVar{Name: "UPSTREAM", Value: "https://app:s3cret-c@db"}, //nolint:gosec // a fake credential to hide
		corev1.EnvVar{Name: "OLD", Value: "x"},
	)
	desired := template(
		corev1.EnvVar{Name: "MODE", Value: "healthy"},
		corev1.EnvVar{Name: "DB_PASSWORD", Value: "s3cret-b"},
		corev1.EnvVar{Name: "UPSTREAM", Value: "https://app:s3cret-d@db"}, //nolint:gosec // a fake credential to hide
		corev1.EnvVar{Name: "NEW", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password",
		}}},
	)

	changes := rollback.Changes(pre, desired)
	for field, want := range map[string][2]string{
		"env MODE":        {"leak", "healthy"},
		"env DB_PASSWORD": {"hidden (sensitive name)", "hidden (sensitive name)"},
		"env UPSTREAM":    {"hidden (looks like a secret)", "hidden (looks like a secret)"},
		"env OLD":         {"x", "not set"},
		"env NEW":         {"not set", "from secret db/password"},
	} {
		c, ok := find(changes, field)
		if !ok || c.Before != want[0] || c.After != want[1] || c.Container != "app" {
			t.Errorf("%s = %+v, %v; want %v", field, c, ok, want)
		}
	}
	for _, c := range changes {
		if strings.Contains(c.Before+c.After, "s3cret") {
			t.Errorf("a change shows a secret: %+v", c)
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

// Args can carry secrets in too many shapes to show them.
func TestChanges_ArgsNeverShown(t *testing.T) {
	pre, desired := template(), template()
	pre.Spec.Containers[0].Args = []string{"--workers=2", "--db-password", "hunter2"}
	desired.Spec.Containers[0].Args = []string{"--workers=4"}
	if c, ok := find(rollback.Changes(pre, desired), "args"); !ok || c.Before != "changed" || c.After != "changed" {
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

// A random token under an innocent name is still hidden.
func TestChanges_RandomValuesHidden(t *testing.T) {
	for value, hidden := range map[string]bool{
		"8f3kQz7LmP2xV9rT4nWb6yHc":            true,
		"healthy":                             false,
		"https://api.example.com/v1/checkout": false,
		"1048576":                             false,
	} {
		pre := template(corev1.EnvVar{Name: "REF", Value: "old"})
		desired := template(corev1.EnvVar{Name: "REF", Value: value})
		c, _ := find(rollback.Changes(pre, desired), "env REF")
		if got := c.After == "hidden (looks like a secret)"; got != hidden {
			t.Errorf("%q: hidden = %v, want %v", value, got, hidden)
		}
	}
}
