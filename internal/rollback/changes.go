package rollback

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// Change is one difference between the current and the restored template.
// Env values, commands and args are never shown: they often hold secrets.
type Change struct {
	Container string `json:"container"`
	Field     string `json:"field"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

// handled are the container fields Changes describes one by one.
var handled = []string{"image", "resources", "command", "args", "env"}

// Changes lists what restoring desired over pre changes: container fields one
// by one, then every other path of the template that differs, values hidden.
func Changes(pre, desired corev1.PodTemplateSpec) []Change {
	out := containerChanges(pre, desired)
	a, b := withoutHandled(pre), withoutHandled(desired)
	return append(out, otherChanges("", a, b)...)
}

func containerChanges(pre, desired corev1.PodTemplateSpec) []Change {
	var out []Change
	before := containersByName(pre)
	after := containersByName(desired)
	for _, name := range names(pre, desired) {
		b, inBefore := before[name]
		a, inAfter := after[name]
		switch {
		case !inBefore:
			out = append(out, Change{name, "container", "not present", "added"})
			continue
		case !inAfter:
			out = append(out, Change{name, "container", "present", "removed"})
			continue
		}
		add := func(field, bv, av string) {
			if bv != av {
				out = append(out, Change{name, field, bv, av})
			}
		}
		add("image", b.Image, a.Image)
		add("limits", quantitiesJSON(b.Resources.Limits), quantitiesJSON(a.Resources.Limits))
		add("requests", quantitiesJSON(b.Resources.Requests), quantitiesJSON(a.Resources.Requests))
		if !slices.Equal(b.Command, a.Command) {
			out = append(out, Change{name, "command", "changed", "changed"})
		}
		if !slices.Equal(b.Args, a.Args) {
			out = append(out, Change{name, "args", "changed", "changed"})
		}
		out = append(out, envChanges(name, b.Env, a.Env)...)
	}
	return out
}

func envChanges(container string, before, after []corev1.EnvVar) []Change {
	b, a := envByName(before), envByName(after)
	var out []Change
	for _, e := range before {
		if v, ok := a[e.Name]; !ok {
			out = append(out, Change{container, "env " + e.Name, "set", "removed"})
		} else if !sameEnv(e, v) {
			out = append(out, Change{container, "env " + e.Name, "changed", "changed"})
		}
	}
	for _, e := range after {
		if _, ok := b[e.Name]; !ok {
			out = append(out, Change{container, "env " + e.Name, "not set", "set"})
		}
	}
	return out
}

func sameEnv(a, b corev1.EnvVar) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func envByName(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

func containersByName(t corev1.PodTemplateSpec) map[string]corev1.Container {
	m := map[string]corev1.Container{}
	for _, c := range t.Spec.Containers {
		m[c.Name] = c
	}
	return m
}

// names lists every container name, in template order.
func names(pre, desired corev1.PodTemplateSpec) []string {
	var out []string
	for _, t := range []corev1.PodTemplateSpec{pre, desired} {
		for _, c := range t.Spec.Containers {
			if !slices.Contains(out, c.Name) {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

func quantitiesJSON(l corev1.ResourceList) string {
	m := make(map[string]string, len(l))
	for k, v := range l {
		m[string(k)] = v.String()
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// withoutHandled is t as JSON, with containers keyed by name and the fields
// containerChanges describes removed.
func withoutHandled(t corev1.PodTemplateSpec) map[string]any {
	var m map[string]any
	b, _ := json.Marshal(t)
	_ = json.Unmarshal(b, &m)
	spec, _ := m["spec"].(map[string]any)
	if list, ok := spec["containers"].([]any); ok {
		byName := map[string]any{}
		for _, c := range list {
			if c, ok := c.(map[string]any); ok {
				for _, f := range handled {
					delete(c, f)
				}
				byName[fmt.Sprint(c["name"])] = c
			}
		}
		spec["containers"] = byName
	}
	return m
}

// otherChanges lists the paths under which a and b differ. Lists are
// compared whole.
func otherChanges(path string, a, b any) []Change {
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	if !okA || !okB {
		if reflect.DeepEqual(a, b) {
			return nil
		}
		return []Change{{Field: path, Before: "changed", After: "changed"}}
	}
	keys := make([]string, 0, len(ma)+len(mb))
	for k := range ma {
		keys = append(keys, k)
	}
	for k := range mb {
		if _, ok := ma[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out []Change
	for _, k := range keys {
		sub := k
		if path != "" {
			sub = path + "." + k
		}
		out = append(out, otherChanges(sub, ma[k], mb[k])...)
	}
	return out
}
