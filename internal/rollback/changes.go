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

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/evidence"
)

// Change is one difference between the current and the restored template.
// Values that look secret, by name or content, are hidden.
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
			out = append(out, Change{name, "command", argv(b.Command), argv(a.Command)})
		}
		if !slices.Equal(b.Args, a.Args) {
			out = append(out, Change{name, "args", argv(b.Args), argv(a.Args)})
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
			out = append(out, Change{container, "env " + e.Name, envValue(e), "not set"})
		} else if !sameEnv(e, v) {
			out = append(out, Change{container, "env " + e.Name, envValue(e), envValue(v)})
		}
	}
	for _, e := range after {
		if _, ok := b[e.Name]; !ok {
			out = append(out, Change{container, "env " + e.Name, "not set", envValue(e)})
		}
	}
	return out
}

// sensitiveName matches env names that usually hold secrets.
var sensitiveName = regexp.MustCompile(`(?i)pass|secret|token|key|credential|auth|private|cert|cookie|session|dsn`)

// maxShown bounds a value shown to an approver.
const maxShown = 120

// envValue shows an env value unless it looks secret; a reference is shown
// as the reference, never resolved.
func envValue(e corev1.EnvVar) string {
	if r := e.ValueFrom; r != nil {
		switch {
		case r.SecretKeyRef != nil:
			return "from secret " + r.SecretKeyRef.Name + "/" + r.SecretKeyRef.Key
		case r.ConfigMapKeyRef != nil:
			return "from configmap " + r.ConfigMapKeyRef.Name + "/" + r.ConfigMapKeyRef.Key
		case r.FieldRef != nil:
			return "from field " + r.FieldRef.FieldPath
		case r.ResourceFieldRef != nil:
			return "from resource " + r.ResourceFieldRef.Resource
		}
		return "from a reference"
	}
	if sensitiveName.MatchString(e.Name) {
		return "hidden (sensitive name)"
	}
	return shown(e.Value)
}

// argv shows arguments one by one, hiding a sensitive flag's value whether it
// follows "=" or comes as the next argument, and both halves of a secret
// split across two arguments, such as "Bearer" and its token. Each
// argument's fate is decided once: shown, shown as a flag name, or hidden.
func argv(args []string) string {
	const (
		show = iota
		name // the flag name stays; its value is the next argument
		hide
	)
	fate := make([]int, len(args))
	// introduces is set for a sensitive flag without "=": what follows it is
	// secret, even when the flag is itself hidden as a value.
	introduces := make([]bool, len(args))
	for i, a := range args {
		introduces[i] = sensitiveFlag(a) && !strings.Contains(a, "=")
		switch {
		case i > 0 && introduces[i-1] && fate[i-1] != show:
			fate[i] = hide
		case looksSecret(a):
			fate[i] = hide
		case sensitiveFlag(a) && strings.Contains(a, "="):
			fate[i] = hide
		case sensitiveFlag(a):
			fate[i] = name
		}
	}
	for i := 0; i+1 < len(args); i++ {
		if fate[i] == show && fate[i+1] == show && looksSecret(args[i]+" "+args[i+1]) {
			fate[i], fate[i+1] = hide, hide
		}
	}
	out := make([]string, len(args))
	for i, a := range args {
		value := i > 0 && introduces[i-1] && fate[i-1] != show
		switch {
		case fate[i] == hide && !value && sensitiveFlag(a) && strings.Contains(a, "="):
			flag, _, _ := strings.Cut(a, "=")
			out[i] = flag + "=<hidden>"
		case fate[i] == hide:
			out[i] = "<hidden>"
		default:
			out[i] = a
		}
	}
	return truncate(strings.Join(out, " "))
}

// sensitiveFlag is a flag naming a secret, or a bare word that introduces
// one: a value following it is secret.
func sensitiveFlag(a string) bool {
	flag, _, _ := strings.Cut(a, "=")
	word := strings.TrimRight(strings.ToLower(flag), ":")
	return (strings.HasPrefix(a, "-") || strings.HasSuffix(flag, ":") || word == "bearer" || word == "basic") &&
		(sensitiveName.MatchString(flag) || word == "bearer" || word == "basic")
}

func looksSecret(s string) bool {
	return shown(s) != truncate(s)
}

// shown is s, unless the evidence redaction rules find a secret in it.
func shown(s string) string {
	if s == "" {
		return `""`
	}
	if _, n := evidence.Redact([]byte(strconv.Quote(s)), evidence.DefaultRules); n > 0 || random(s) {
		return "hidden (looks like a secret)"
	}
	return truncate(s)
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
