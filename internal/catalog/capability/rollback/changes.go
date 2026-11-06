package rollback

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/platform/redact"

	corev1 "k8s.io/api/core/v1"
)

// Change is one difference between the current and the restored template.
// Env values that look secret, by name or content, are hidden; commands and
// args are never shown, since secrets hide in them in too many shapes.
type Change = capability.Change

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
			out = append(out, Change{Container: name, Field: "container", Before: "not present", After: "added"})
			continue
		case !inAfter:
			out = append(out, Change{Container: name, Field: "container", Before: "present", After: "removed"})
			continue
		}
		add := func(field, bv, av string) {
			if bv != av {
				out = append(out, Change{Container: name, Field: field, Before: bv, After: av})
			}
		}
		add("image", b.Image, a.Image)
		add("limits", quantitiesJSON(b.Resources.Limits), quantitiesJSON(a.Resources.Limits))
		add("requests", quantitiesJSON(b.Resources.Requests), quantitiesJSON(a.Resources.Requests))
		if !slices.Equal(b.Command, a.Command) {
			out = append(out, Change{Container: name, Field: "command", Before: "changed", After: "changed", Withheld: capability.Summary})
		}
		if !slices.Equal(b.Args, a.Args) {
			out = append(out, Change{Container: name, Field: "args", Before: "changed", After: "changed", Withheld: capability.Summary})
		}
		out = append(out, envChanges(name, b.Env, a.Env)...)
	}
	return out
}

func envChanges(container string, before, after []corev1.EnvVar) []Change {
	b, a := envByName(before), envByName(after)
	var out []Change
	add := func(name string, bv, av envShown) {
		c := Change{Container: container, Field: "env " + name, Before: bv.value, After: av.value}
		if bv.hidden || av.hidden {
			c.Withheld = capability.Secret
		}
		out = append(out, c)
	}
	unset := envShown{value: "not set"}
	for _, e := range before {
		if v, ok := a[e.Name]; !ok {
			add(e.Name, envValue(e), unset)
		} else if !sameEnv(e, v) {
			add(e.Name, envValue(e), envValue(v))
		}
	}
	for _, e := range after {
		if _, ok := b[e.Name]; !ok {
			add(e.Name, unset, envValue(e))
		}
	}
	return out
}

// sensitiveName matches env names that usually hold secrets.
var sensitiveName = regexp.MustCompile(`(?i)pass|secret|token|key|credential|auth|private|cert|cookie|session|dsn`)

// maxShown bounds a value shown to an approver.
const maxShown = 120

// envShown is an env value as the approver sees it, and whether it is
// hidden.
type envShown struct {
	value  string
	hidden bool
}

// envValue shows an env value unless it looks secret; a reference is shown
// as the reference, never resolved.
func envValue(e corev1.EnvVar) envShown {
	if r := e.ValueFrom; r != nil {
		switch {
		case r.SecretKeyRef != nil:
			return envShown{value: "from secret " + r.SecretKeyRef.Name + "/" + r.SecretKeyRef.Key}
		case r.ConfigMapKeyRef != nil:
			return envShown{value: "from configmap " + r.ConfigMapKeyRef.Name + "/" + r.ConfigMapKeyRef.Key}
		case r.FieldRef != nil:
			return envShown{value: "from field " + r.FieldRef.FieldPath}
		case r.ResourceFieldRef != nil:
			return envShown{value: "from resource " + r.ResourceFieldRef.Resource}
		}
		return envShown{value: "from a reference"}
	}
	if sensitiveName.MatchString(e.Name) {
		return envShown{value: "hidden (sensitive name)", hidden: true}
	}
	return shown(e.Value)
}

// shown is s, unless the redaction rules find a secret in it.
func shown(s string) envShown {
	if s == "" {
		return envShown{value: `""`}
	}
	if _, n := redact.Apply([]byte(strconv.Quote(s)), redact.DefaultRules); n > 0 || random(s) {
		return envShown{value: "hidden (looks like a secret)", hidden: true}
	}
	return envShown{value: truncate(s)}
}

// random reports a long, unbroken value with high entropy, as tokens and
// keys are; words, URLs and numbers fall below it.
func random(s string) bool {
	if len(s) < 16 || strings.ContainsAny(s, " /") {
		return false
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	entropy := 0.0
	for _, c := range counts {
		p := float64(c) / n
		entropy -= p * math.Log2(p)
	}
	return entropy > 3.5
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxShown {
		return string(r[:maxShown]) + "…"
	}
	return s
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
		return []Change{{Field: path, Before: "changed", After: "changed", Withheld: capability.Summary}}
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
