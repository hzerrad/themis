package probes_test

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/catalog/capability/probes"
)

// Words name a change to one field by what it does to it, and a change to
// several by the probes they belong to.
func TestWords(t *testing.T) {
	template := func(liveness, readiness *corev1.Probe) json.RawMessage {
		b, err := json.Marshal(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", LivenessProbe: liveness, ReadinessProbe: readiness}}}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	probe := func(timeout, failures int32) *corev1.Probe {
		return &corev1.Probe{TimeoutSeconds: timeout, FailureThreshold: failures}
	}
	for name, tc := range map[string]struct {
		before, after json.RawMessage
		params        map[string]any
		act, verifies string
	}{
		"a longer timeout": {
			before: template(probe(1, 3), nil), after: template(probe(3, 3), nil),
			act: "Lengthen inventory's liveness probe timeout from 1s to 3s", verifies: "the pods become ready and stay ready, with no restart",
		},
		"a lower failure threshold": {
			before: template(nil, probe(1, 5)), after: template(nil, probe(1, 2)),
			act: "Lower inventory's readiness probe failure threshold from 5 to 2", verifies: "the pods become ready and stay ready",
		},
		"two fields of one probe": {
			before: template(probe(1, 3), nil), after: template(probe(3, 5), nil),
			act: "Change inventory's liveness probe", verifies: "the pods become ready and stay ready, with no restart",
		},
		"two probes": {
			before: template(probe(1, 3), probe(1, 3)), after: template(probe(2, 3), probe(2, 3)),
			act: "Change inventory's liveness and readiness probes", verifies: "the pods become ready and stay ready, with no restart",
		},
		"before it is worked out": {
			params: map[string]any{"container": "app", "readiness": map[string]any{"periodSeconds": int64(20)}},
			act:    "Set the readiness probe period of inventory's app container to 20s", verifies: "the pods become ready and stay ready",
		},
	} {
		got := (probes.Capability{}).Words("inventory", tc.params, tc.before, tc.after)
		if got.Noun != "probe change" || got.Act != tc.act || got.Verifies != tc.verifies {
			t.Errorf("%s: %+v, want %q verifying %q", name, got, tc.act, tc.verifies)
		}
	}
	if got := (probes.Capability{}).Words("inventory", map[string]any{}, nil, nil); got != capability.Say("probe change", capability.VerbChange, "the probes of inventory", capability.PodsStayReady) {
		t.Errorf("no params: %+v", got)
	}
}
