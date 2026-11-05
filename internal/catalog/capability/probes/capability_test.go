package probes_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
)

// Validate takes a container and the fields to set on its probes, as whole
// numbers Kubernetes accepts, and nothing else.
func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		want   map[string]any
	}{
		"as decoded from JSON": {
			params: map[string]any{"container": "app", "readiness": map[string]any{"timeoutSeconds": 5.0, "successThreshold": 2.0}, "liveness": map[string]any{"initialDelaySeconds": 0.0}},
			want:   map[string]any{"container": "app", "readiness": map[string]any{"timeoutSeconds": int64(5), "successThreshold": int64(2)}, "liveness": map[string]any{"initialDelaySeconds": int64(0)}},
		},
		"no container":                   {params: map[string]any{"readiness": map[string]any{"timeoutSeconds": 5.0}}},
		"no probe":                       {params: map[string]any{"container": "app"}},
		"not a probe":                    {params: map[string]any{"container": "app", "httpGet": map[string]any{"path": "/"}}},
		"an empty probe":                 {params: map[string]any{"container": "app", "startup": map[string]any{}}},
		"what the probe calls":           {params: map[string]any{"container": "app", "liveness": map[string]any{"exec": "true"}}},
		"successThreshold of a liveness": {params: map[string]any{"container": "app", "liveness": map[string]any{"successThreshold": 2.0}}},
		"zero period":                    {params: map[string]any{"container": "app", "liveness": map[string]any{"periodSeconds": 0.0}}},
		"negative delay":                 {params: map[string]any{"container": "app", "liveness": map[string]any{"initialDelaySeconds": -1.0}}},
		"a fraction":                     {params: map[string]any{"container": "app", "liveness": map[string]any{"timeoutSeconds": 1.5}}},
		"beyond what Kubernetes accepts": {params: map[string]any{"container": "app", "liveness": map[string]any{"failureThreshold": 3e9}}},
		"a number as text":               {params: map[string]any{"container": "app", "liveness": map[string]any{"failureThreshold": "3"}}},
	} {
		got, err := probes.Capability{}.Validate(tc.params)
		if tc.want == nil {
			if !errors.Is(err, capability.ErrInvalidParams) {
				t.Errorf("%s: err = %v, want ErrInvalidParams", name, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Validate = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// The schema an investigator reads is valid JSON and asks for a container.
func TestSchema(t *testing.T) {
	var s struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(probes.Capability{}.Schema(), &s); err != nil || len(s.Required) != 1 || len(s.Properties) != 4 {
		t.Errorf("schema = %+v, %v", s, err)
	}
}

func withProbes(liveness, readiness *corev1.Probe) json.RawMessage {
	b, _ := json.Marshal(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "app", LivenessProbe: liveness, ReadinessProbe: readiness},
	}}})
	return b
}

// The approver sees every field that changes and which way it moves the
// probe, and a more lenient readiness probe on a line of its own. A field
// left at zero is shown as the default Kubernetes runs it with.
func TestChanges(t *testing.T) {
	before := withProbes(&corev1.Probe{PeriodSeconds: 10}, &corev1.Probe{TimeoutSeconds: 1})
	after := withProbes(&corev1.Probe{PeriodSeconds: 5}, &corev1.Probe{TimeoutSeconds: 5, FailureThreshold: 3})
	got, err := probes.Capability{}.Changes(before, after)
	if err != nil {
		t.Fatal(err)
	}
	want := []capability.Change{
		{Container: "app", Field: "liveness probe periodSeconds", Before: "10", After: "5 (stricter)"},
		{Container: "app", Field: "readiness probe", Before: "as it is", After: "more lenient: pods that fail it keep receiving traffic for longer"},
		{Container: "app", Field: "readiness probe timeoutSeconds", Before: "1", After: "5 (more lenient)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Changes =\n%+v\nwant\n%+v", got, want)
	}
	if _, err := (probes.Capability{}).Changes(json.RawMessage(`[]`), after); err == nil {
		t.Error("Changes read a template that isn't one")
	}
}
