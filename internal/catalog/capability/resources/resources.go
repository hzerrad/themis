// Package resources is the deployment.set-resources capability: it sets the
// memory and CPU requests and limits of named containers in a Deployment's
// pod template, to values the investigator chose from evidence. It checks the
// values are valid and that the cluster can run them, never how big they are:
// policy and a person decide that.
package resources

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/hzerrad/themis/internal/catalog/capability"
)

const (
	Name    = "deployment.set-resources"
	Version = "1"
)

// field is one value the fix can set: a request or a limit of a resource.
type field struct {
	kind     string // requests or limits
	resource corev1.ResourceName
}

// fields are every value the fix can set, in the order they are shown.
var fields = []field{
	{"requests", corev1.ResourceMemory},
	{"limits", corev1.ResourceMemory},
	{"requests", corev1.ResourceCPU},
	{"limits", corev1.ResourceCPU},
}

func (f field) String() string {
	return resourceName(f.resource) + " " + strings.TrimSuffix(f.kind, "s")
}

func resourceName(r corev1.ResourceName) string {
	if r == corev1.ResourceCPU {
		return "CPU"
	}
	return string(r)
}

// list is the container's list f is in.
func (f field) list(c *corev1.Container) *corev1.ResourceList {
	if f.kind == "requests" {
		return &c.Resources.Requests
	}
	return &c.Resources.Limits
}

// get is f's value in c, or nil when it isn't set.
func (f field) get(c *corev1.Container) *resource.Quantity {
	if q, ok := (*f.list(c))[f.resource]; ok {
		return &q
	}
	return nil
}

func (f field) set(c *corev1.Container, q resource.Quantity) {
	l := f.list(c)
	if *l == nil {
		*l = corev1.ResourceList{}
	}
	(*l)[f.resource] = q
}

// asked is the values params set, by container name.
type asked map[string]map[field]resource.Quantity

// parse reads params, as a suggestion gives them or as the fix stores them.
func parse(params map[string]any) (asked, error) {
	containers, ok := params["containers"].(map[string]any)
	if !ok || len(params) != 1 || len(containers) == 0 {
		return nil, invalid("set-resources needs only containers: each container's name, with the requests or limits to set")
	}
	out := asked{}
	for name, v := range containers {
		kinds, ok := v.(map[string]any)
		if name == "" || !ok || len(kinds) == 0 {
			return nil, invalid("container %q needs requests or limits to set", name)
		}
		out[name] = map[field]resource.Quantity{}
		for kind, v := range kinds {
			values, ok := v.(map[string]any)
			if (kind != "requests" && kind != "limits") || !ok || len(values) == 0 {
				return nil, invalid("container %q: only requests and limits can be set, each with memory or cpu", name)
			}
			for res, v := range values {
				f := field{kind, corev1.ResourceName(res)}
				if !slices.Contains(fields, f) {
					return nil, invalid("container %q: only memory and cpu can be set, not %q", name, res)
				}
				s, _ := v.(string)
				q, err := resource.ParseQuantity(s)
				if err != nil {
					return nil, invalid("container %q: the %s %v is not a Kubernetes quantity such as 512Mi or 250m", name, f, v)
				}
				if q.Sign() <= 0 {
					return nil, invalid("container %q: the %s must be more than zero", name, f)
				}
				out[name][f] = q
			}
		}
	}
	return out, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", capability.ErrInvalidParams, fmt.Sprintf(format, args...))
}

// params are a as the fix stores them, each value in its canonical form.
func (a asked) params() map[string]any {
	containers := map[string]any{}
	for name, values := range a {
		kinds := map[string]any{}
		for f, q := range values {
			if kinds[f.kind] == nil {
				kinds[f.kind] = map[string]any{}
			}
			kinds[f.kind].(map[string]any)[string(f.resource)] = q.String() //nolint:forcetypeassert // set just above
		}
		containers[name] = kinds
	}
	return map[string]any{"containers": containers}
}

// diff is one value the fix changes; nil means not set.
type diff struct {
	container     string
	field         field
	before, after *resource.Quantity
}

// diffs lists the values that differ between before and after, for the
// containers both have, in template order.
func diffs(before, after corev1.PodTemplateSpec) []diff {
	var out []diff
	for _, a := range after.Spec.Containers {
		i := slices.IndexFunc(before.Spec.Containers, func(c corev1.Container) bool { return c.Name == a.Name })
		if i < 0 {
			continue
		}
		for _, f := range fields {
			b, v := f.get(&before.Spec.Containers[i]), f.get(&a)
			if !equal(b, v) {
				out = append(out, diff{a.Name, f, b, v})
			}
		}
	}
	return out
}

func equal(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(*b) == 0
}

func shown(q *resource.Quantity) string {
	if q == nil {
		return "not set"
	}
	return q.String()
}

func (d diff) change() capability.Change {
	return capability.Change{Container: d.container, Field: d.field.String(), Before: shown(d.before), After: shown(d.after)}
}

// facts are what policy sees of the diffs, as change.<key>:
//
//	containers                    the containers changed, comma-separated
//	memory, cpu                   increase, decrease or mixed, over every value of it changed
//	memory.ratio, cpu.ratio       the largest new value over its old one
//	<container>.<memory|cpu>.<request|limit>
//	                              increase, decrease, or set where there was none
//	  ….before, ….after           the old value (or "not set") and the new one
//	  ….ratio                     the new value over the old one, when there was one
func facts(ds []diff) map[string]string {
	out := map[string]string{}
	var containers []string
	largest := map[corev1.ResourceName]float64{}
	for _, d := range ds {
		if !slices.Contains(containers, d.container) {
			containers = append(containers, d.container)
		}
		res := string(d.field.resource)
		key := d.container + "." + res + "." + strings.TrimSuffix(d.field.kind, "s")
		out[key+".before"], out[key+".after"] = shown(d.before), shown(d.after)
		way := "increase"
		switch {
		case d.before == nil:
			out[key] = "set"
		case d.after.Cmp(*d.before) < 0:
			way, out[key] = "decrease", "decrease"
		default:
			out[key] = "increase"
		}
		if seen, ok := out[res]; ok && seen != way {
			way = "mixed"
		}
		out[res] = way
		if d.before == nil {
			continue
		}
		ratio := math.Round(d.after.AsApproximateFloat64()/d.before.AsApproximateFloat64()*100) / 100
		out[key+".ratio"] = strconv.FormatFloat(ratio, 'f', -1, 64)
		if r, ok := largest[d.field.resource]; !ok || ratio > r {
			largest[d.field.resource] = ratio
			out[res+".ratio"] = out[key+".ratio"]
		}
	}
	out["containers"] = strings.Join(containers, ",")
	return out
}

// done says what the fix did, once applied.
func done(ds []diff) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		parts = append(parts, fmt.Sprintf("the %s of %s to %s", d.field, d.container, shown(d.after)))
	}
	return "Set " + strings.Join(parts, ", ") + "."
}

// names lists a template's containers, for saying which there are.
func names(t corev1.PodTemplateSpec) string {
	var out []string
	for _, c := range t.Spec.Containers {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

// sortedNames are a's container names, so refusals come in a stable order.
func (a asked) sortedNames() []string { return slices.Sorted(maps.Keys(a)) }
