package rollback

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/hzerrad/themis/internal/catalog/capability"
	"github.com/hzerrad/themis/internal/incident"
)

var (
	_ capability.Commanding = Capability{}
	_ capability.Commanding = Partial{}
)

// Commands roll the workload back to the revision, as kubectl does: it
// writes that revision's template, as the rollback would.
func (Capability) Commands(t incident.TargetRef, params map[string]any, _, _ json.RawMessage) []string {
	revision, ok := RevisionParam(params)
	if !ok {
		return nil
	}
	return []string{capability.Kubectl(t, "rollout undo", "--to-revision="+strconv.FormatInt(revision, 10))}
}

// Commands set the images, environment variables and resources the partial
// rollback puts back, container by container. None when it changes anything
// else, such as a command, or an environment variable that comes from a
// reference or looks secret.
func (Partial) Commands(t incident.TargetRef, _ map[string]any, before, after json.RawMessage) []string {
	var pre partialBefore
	var desired corev1.PodTemplateSpec
	if json.Unmarshal(before, &pre) != nil || json.Unmarshal(after, &desired) != nil {
		return nil
	}
	return setCommands(t, pre.Template, desired)
}

// setCommands are the kubectl set commands that write desired over pre, or
// none when kubectl set can't.
func setCommands(t incident.TargetRef, pre, desired corev1.PodTemplateSpec) []string {
	if !reflect.DeepEqual(withoutSettable(pre), withoutSettable(desired)) {
		return nil
	}
	before := containersByName(pre)
	var images, out []string
	for _, a := range desired.Spec.Containers {
		b := before[a.Name]
		if a.Image != b.Image {
			images = append(images, capability.Quote(a.Name+"="+a.Image))
		}
		env, ok := envArgs(b.Env, a.Env)
		if !ok {
			return nil
		}
		if len(env) > 0 {
			out = append(out, capability.Kubectl(t, "set env", append([]string{"-c", a.Name}, env...)...))
		}
		res, ok := resourceArgs(b.Resources, a.Resources)
		if !ok {
			return nil
		}
		if len(res) > 0 {
			out = append(out, capability.Kubectl(t, "set resources", append([]string{"-c", a.Name}, res...)...))
		}
	}
	if len(images) > 0 {
		out = append([]string{capability.Kubectl(t, "set image", images...)}, out...)
	}
	return out
}

// withoutSettable is t without what kubectl set changes: each container's
// image, environment and resources.
func withoutSettable(t corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	out := *t.DeepCopy()
	for i := range out.Spec.Containers {
		c := &out.Spec.Containers[i]
		c.Image, c.Env, c.Resources = "", nil, corev1.ResourceRequirements{}
	}
	return out
}

// envArgs are kubectl set env's arguments that turn before into after:
// NAME=value for each value set, NAME- for each removed. kubectl keeps the
// order of the variables it updates and adds new ones last, so after must
// list them that way. Not ok otherwise, or when a value set comes from a
// reference, or replaces one, or either looks secret, or spans lines.
func envArgs(before, after []corev1.EnvVar) ([]string, bool) {
	b, a := envByName(before), envByName(after)
	if len(b) != len(before) || len(a) != len(after) {
		return nil, false
	}
	var out, kept, added []string
	for _, e := range before {
		if _, ok := a[e.Name]; ok {
			kept = append(kept, e.Name)
		} else {
			out = append(out, capability.Quote(e.Name+"-"))
		}
	}
	var set []string
	for _, e := range after {
		old, ok := b[e.Name]
		if !ok {
			added = append(added, e.Name)
		}
		if ok && sameEnv(old, e) {
			continue
		}
		if e.ValueFrom != nil || old.ValueFrom != nil || envValue(e).hidden || (ok && envValue(old).hidden) || strings.ContainsAny(e.Value, "\r\n") {
			return nil, false
		}
		set = append(set, capability.Quote(e.Name+"="+e.Value))
	}
	order := make([]string, 0, len(after))
	for _, e := range after {
		order = append(order, e.Name)
	}
	if !slices.Equal(append(kept, added...), order) {
		return nil, false
	}
	return append(set, out...), true
}

// resourceArgs are kubectl set resources' arguments that turn before into
// after. Not ok when after drops a value before has: kubectl set can't.
func resourceArgs(before, after corev1.ResourceRequirements) ([]string, bool) {
	var out []string
	for _, l := range []struct {
		flag          string
		before, after corev1.ResourceList
	}{{"--limits", before.Limits, after.Limits}, {"--requests", before.Requests, after.Requests}} {
		var set []string
		for name := range l.before {
			if _, ok := l.after[name]; !ok {
				return nil, false
			}
		}
		for _, name := range slices.Sorted(maps.Keys(l.after)) {
			q := l.after[name]
			if old, ok := l.before[name]; !ok || old.Cmp(q) != 0 {
				set = append(set, string(name)+"="+q.String())
			}
		}
		if len(set) > 0 {
			out = append(out, capability.Quote(l.flag+"="+strings.Join(set, ",")))
		}
	}
	return out, true
}
