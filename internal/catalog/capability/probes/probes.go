// Package probes is the deployment.set-probes capability: it changes the
// timing of a container's liveness, readiness and startup probes, such as how
// long a check may take or how many failures it tolerates. It never adds or
// removes a probe, or changes what the probe calls: those are application
// changes, for a person. The investigator chooses the values, citing the
// evidence; policy and a person decide.
package probes

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

const (
	Name    = "deployment.set-probes"
	Version = "1"
)

// The probes a container can have, in the order they are shown.
var kinds = []string{"liveness", "readiness", "startup"}

// The timing fields of a probe, in the order they are shown.
var fields = []string{"initialDelaySeconds", "periodSeconds", "timeoutSeconds", "failureThreshold", "successThreshold"}

// fieldsOf are the fields params may set on a kind of probe: Kubernetes
// requires successThreshold to be 1 for liveness and startup probes.
func fieldsOf(kind string) []string {
	if kind == "readiness" {
		return fields
	}
	return slices.DeleteFunc(slices.Clone(fields), func(f string) bool { return f == "successThreshold" })
}

// defaults are what Kubernetes uses for a field left at zero.
var defaults = map[string]int32{"periodSeconds": 10, "timeoutSeconds": 1, "failureThreshold": 3, "successThreshold": 1}

// minimum is the smallest value Kubernetes accepts for a field, apart from
// zero meaning its default.
func minimum(field string) int64 {
	if field == "initialDelaySeconds" {
		return 0
	}
	return 1
}

// request is what params ask for: a container, and for each probe to change,
// the fields to set.
type request struct {
	container string
	set       map[string]map[string]int32
}

// params is r as a fix stores it.
func (r request) params() map[string]any {
	out := map[string]any{"container": r.container}
	for kind, values := range r.set {
		m := map[string]any{}
		for f, v := range values {
			m[f] = int64(v)
		}
		out[kind] = m
	}
	return out
}

// parse reads params. Strict, it refuses keys it doesn't know; otherwise it
// ignores them, such as those an action adds to bind what it restores.
func parse(params map[string]any, strict bool) (request, error) {
	bad := func(format string, a ...any) (request, error) {
		return request{}, fmt.Errorf("%w: "+format, append([]any{capability.ErrInvalidParams}, a...)...)
	}
	container, ok := params["container"].(string)
	if !ok || container == "" {
		return bad("container must name the container whose probes to change")
	}
	r := request{container: container, set: map[string]map[string]int32{}}
	for key, value := range params {
		if key == "container" {
			continue
		}
		if !slices.Contains(kinds, key) {
			if strict {
				return bad("%s is not a probe; the probes are liveness, readiness and startup", key)
			}
			continue
		}
		values, ok := value.(map[string]any)
		if !ok || len(values) == 0 {
			return bad("%s must set at least one of %s", key, strings.Join(fieldsOf(key), ", "))
		}
		r.set[key] = map[string]int32{}
		for _, f := range slices.Sorted(maps.Keys(values)) {
			if !slices.Contains(fieldsOf(key), f) {
				if f == "successThreshold" {
					return bad("Kubernetes requires successThreshold to be 1 for a %s probe, so it can't be changed", key)
				}
				return bad("%s can't be set on a probe; the fields are %s", f, strings.Join(fieldsOf(key), ", "))
			}
			n, ok := whole(values[f])
			if !ok || n < minimum(f) || n > math.MaxInt32 {
				return bad("%s.%s must be a whole number from %d to %d", key, f, minimum(f), math.MaxInt32)
			}
			r.set[key][f] = int32(n) //nolint:gosec // checked against MaxInt32 above
		}
	}
	if len(r.set) == 0 {
		return bad("say which probe to change: liveness, readiness or startup")
	}
	return r, nil
}

// whole is v as a whole number, as stored (int64) or decoded from JSON
// (float64).
func whole(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), n == math.Trunc(n) && !math.IsInf(n, 0)
	}
	return 0, false
}

// probe is the container's probe of kind, or nil.
func probe(c *corev1.Container, kind string) *corev1.Probe {
	switch kind {
	case "liveness":
		return c.LivenessProbe
	case "readiness":
		return c.ReadinessProbe
	case "startup":
		return c.StartupProbe
	}
	return nil
}

// field is the probe's field named.
func field(p *corev1.Probe, name string) *int32 {
	switch name {
	case "initialDelaySeconds":
		return &p.InitialDelaySeconds
	case "periodSeconds":
		return &p.PeriodSeconds
	case "timeoutSeconds":
		return &p.TimeoutSeconds
	case "failureThreshold":
		return &p.FailureThreshold
	default:
		return &p.SuccessThreshold
	}
}

// value is the probe's field as Kubernetes runs it: zero means the default.
func value(p *corev1.Probe, name string) int32 {
	if v := *field(p, name); v != 0 {
		return v
	}
	return defaults[name]
}

// container is the template's container named, or nil.
func container(t *corev1.PodTemplateSpec, name string) *corev1.Container {
	for i := range t.Spec.Containers {
		if t.Spec.Containers[i].Name == name {
			return &t.Spec.Containers[i]
		}
	}
	return nil
}

// lenient reports whether changing field from before to after makes a probe
// more lenient: slower to call a container failed, or quicker to call it
// ready.
func lenient(field string, before, after int32) bool {
	if field == "successThreshold" {
		return after < before
	}
	return after > before
}
