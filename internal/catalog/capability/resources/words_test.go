package resources_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/resources"
)

// Words name a change by what it does to the values: one value with what it
// was and becomes, several by name, and only what params set before the fix
// is worked out.
func TestWords(t *testing.T) {
	template := func(containers ...corev1.Container) json.RawMessage {
		b, err := json.Marshal(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// with is a container with values, such as "limits.cpu": "200m".
	with := func(name string, values map[string]string) corev1.Container {
		c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}}
		for key, v := range values {
			kind, res, _ := strings.Cut(key, ".")
			list := c.Resources.Limits
			if kind == "requests" {
				list = c.Resources.Requests
			}
			list[corev1.ResourceName(res)] = resource.MustParse(v)
		}
		return c
	}
	proxy := corev1.Container{Name: "proxy"}
	for name, tc := range map[string]struct {
		before, after json.RawMessage
		params        map[string]any
		want          capability.Words
	}{
		"a raised CPU limit": {
			before: template(with("app", map[string]string{"limits.cpu": "200m"})), after: template(with("app", map[string]string{"limits.cpu": "300m"})),
			want: capability.Words{
				Noun: "raise", Act: "Raise search's CPU limit from 200m to 300m",
				Doing: "Raising search's CPU limit from 200m to 300m.", Done: "Raised search's CPU limit from 200m to 300m.",
				Verifies: "the pods become ready and stay ready, and are no longer throttled at their CPU limit",
			},
		},
		"a lowered request of one container of two": {
			before: template(with("app", map[string]string{"requests.cpu": "100m"}), proxy), after: template(with("app", map[string]string{"requests.cpu": "50m"}), proxy),
			want: capability.Words{
				Noun: "decrease", Act: "Lower the CPU request of search's app container from 100m to 50m",
				Doing: "Lowering the CPU request of search's app container from 100m to 50m.", Done: "Lowered the CPU request of search's app container from 100m to 50m.",
				Verifies: "the pods become ready and stay ready",
			},
		},
		"memory raised in two containers": {
			before: template(with("app", map[string]string{"limits.memory": "256Mi"}), with("proxy", map[string]string{"limits.memory": "64Mi"})),
			after:  template(with("app", map[string]string{"limits.memory": "512Mi"}), with("proxy", map[string]string{"limits.memory": "128Mi"})),
			want: capability.Words{
				Noun: "raise", Act: "Raise the memory limits of search's app and proxy containers",
				Doing: "Raising the memory limits of search's app and proxy containers.", Done: "Raised the memory limits of search's app and proxy containers.",
				Verifies: "the pods stop being OOMKilled and stay ready",
			},
		},
		"a value set where there was none, and one raised": {
			before: template(with("app", map[string]string{"limits.memory": "256Mi"})),
			after:  template(with("app", map[string]string{"limits.memory": "512Mi", "requests.memory": "256Mi"})),
			want: capability.Words{
				Noun: "change", Act: "Change search's memory request and memory limit",
				Doing: "Changing search's memory request and memory limit.", Done: "Changed search's memory request and memory limit.",
				Verifies: "the pods become ready and stay ready, with no OOM kill",
			},
		},
		"before it is worked out": {
			params: map[string]any{"containers": map[string]any{"app": map[string]any{"limits": map[string]any{"cpu": "300m"}}}},
			want: capability.Words{
				Noun: "change", Act: "Set the CPU limit of search's app container to 300m",
				Doing: "Setting the CPU limit of search's app container to 300m.", Done: "Set the CPU limit of search's app container to 300m.",
				Verifies: "the pods become ready and stay ready",
			},
		},
	} {
		if got := (resources.Capability{}).Words("search", tc.params, tc.before, tc.after); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, tc.want)
		}
	}
}
